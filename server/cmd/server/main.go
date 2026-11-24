// Command server - headless-сервер Inspired Kingdoms (MVP-0).
// Процесс грузит конфиги, восстанавливает снапшот и крутит цикл актора
// симуляции: команды клиентов и события планировщика по таймеру ближайшего
// события (тика нет), периодически сохраняет состояние.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"inspiredtk/server/internal/config"
	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/persistence"
	"inspiredtk/server/internal/sim"
	"inspiredtk/server/internal/transport"
)

// main is the entry point for the game server.
func main() {
	configsDir := flag.String("configs", "configs", "каталог с файлами конфигов")
	flag.Parse()
	logger := log.New(os.Stdout, "inspiredtk ", log.LstdFlags)

	cfg, err := config.Load(*configsDir)
	if err != nil {
		logger.Fatalf("configs: %v", err)
	}

	events, err := persistence.OpenEventLog(cfg.World.EventLogPath)
	if err != nil {
		logger.Fatalf("event log: %v", err)
	}
	defer func() {
		if err := events.Close(); err != nil {
			logger.Printf("close event log: %v", err)
		}
	}()

	snap, err := persistence.LoadSnapshot(cfg.World.SnapshotPath)
	if err != nil {
		logger.Fatalf("snapshot: %v", err)
	}
	var world *domain.World
	if snap != nil {
		world = snap.World
		logger.Printf("snapshot restored: server time %d, %d scheduled events", snap.ServerTimeUnix, len(world.Events))
	} else {
		world = sim.NewWorld(cfg, time.Now().Unix())
		logger.Printf("new world generated: %dx%d, %d barbarian camps", world.MapSize, world.MapSize, len(world.Camps))
	}
	engine := sim.NewEngine(cfg, world, events)

	// Догон после простоя сервера: все просроченные события применяются по
	// порядку (fire_at, seq) до приёма соединений.
	if n := engine.Advance(time.Now().Unix()); n > 0 {
		logger.Printf("scheduler: caught up %d overdue events", n)
	}

	// Сетевой край: у каждого соединения свои горутины чтения и записи;
	// события сессий разбирает цикл ниже, реестр сессий принадлежит ему.
	wsServer := transport.NewServer()
	hub := transport.NewHub()

	// Setup HTTP routes
	http.HandleFunc("/ws", wsServer.HandleWebSocket)

	// Start HTTP server in goroutine
	go func() {
		logger.Printf("WebSocket server starting on :8080")
		if err := http.ListenAndServe(":8080", nil); err != nil {
			logger.Fatalf("WebSocket server failed: %v", err)
		}
	}()

	save := func() {
		s := persistence.NewSnapshot(engine.World, time.Now().Unix())
		if err := persistence.SaveSnapshot(cfg.World.SnapshotPath, s); err != nil {
			logger.Printf("snapshot save: %v", err)
		}
	}

	// Таймер планировщика: взводится на ближайшее событие кучи. Пока событий
	// нет, wakeC равен nil и цикл по нему не просыпается.
	wake := time.NewTimer(time.Hour)
	defer wake.Stop()
	var wakeC <-chan time.Time
	rearm := func() {
		wake.Stop()
		wakeC = nil
		if next, ok := engine.NextEventUnix(); ok {
			wake.Reset(max(time.Until(time.Unix(next, 0)), 0))
			wakeC = wake.C
		}
	}
	rearm()

	// Временная рассылка состояния раз в секунду: debug-клиент не интерполирует
	// ресурсы и прогресс сам. Симуляцию она не двигает и уйдёт вместе с
	// адресной рассылкой (documentation/roadmap.md, «транспорт под модель»).
	pushTick := time.NewTicker(time.Second)
	defer pushTick.Stop()
	snapTick := time.NewTicker(time.Duration(cfg.World.SnapshotIntervalSeconds) * time.Second)
	defer snapTick.Stop()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	logger.Printf("started: event scheduler, snapshot every %ds", cfg.World.SnapshotIntervalSeconds)
	for {
		select {
		case <-wakeC:
			engine.Advance(time.Now().Unix())
			rearm()
		case ev := <-wsServer.Events():
			switch ev.Kind {
			case transport.EventJoin:
				now := time.Now().Unix()
				engine.Advance(now)
				// В debugMode незнакомый игрок создаётся при входе.
				info, err := engine.JoinPlayer(ev.Session.PlayerID, now)
				if err != nil {
					logger.Printf("session %d: join player %q: %v", ev.Session.ID, ev.Session.PlayerID, err)
					ev.Session.SendAndClose(transport.EncodeError(fmt.Sprintf("join: %v", err)))
					break
				}
				hub.Add(ev.Session)
				hub.Send(ev.Session, transport.MessageTypeHello, info)
				hub.Send(ev.Session, transport.MessageTypeState, engine.PlayerState(info.PlayerID, now))
				rearm()
			case transport.EventLeave:
				hub.Remove(ev.Session)
			case transport.EventCommand:
				cmd := ev.Command
				// Инвариант: перед командой применены все события с fire_at <= now.
				engine.Advance(time.Now().Unix())
				if err := executeCommand(engine, hub, ev.Session.PlayerID, cmd); err != nil {
					logger.Printf("command %s failed: %v", cmd.Name, err)
					hub.Broadcast(transport.MessageTypeError, map[string]string{
						"error": fmt.Sprintf("command %s: %v", cmd.Name, err),
					})
				}
				// Команда могла поставить событие раньше прежнего ближайшего
				// (или сдвинуть время в debug-режиме).
				rearm()
			}
		case <-pushTick.C:
			now := time.Now().Unix()
			hub.BroadcastPerPlayer(transport.MessageTypeState, func(playerID string) interface{} {
				return engine.PlayerState(playerID, now)
			})
		case <-snapTick.C:
			save()
		case <-sig:
			save()
			logger.Printf("stopped, final snapshot saved")
			return
		}
	}
}

// executeCommand routes a queued client command to the engine on behalf of
// the session's player. Commands on a settlement carry data.settlementId;
// ownership is checked by the engine.
// Engine is not thread-safe, so this is called only from the main loop.
func executeCommand(e *sim.Engine, hub *transport.Hub, playerID string, cmd transport.Command) error {
	settlementID := func() (string, error) {
		id, ok := cmd.Data["settlementId"].(string)
		if !ok || id == "" {
			return "", fmt.Errorf("missing settlementId parameter")
		}
		return id, nil
	}
	switch cmd.Name {
	case "add_resources":
		sid, err := settlementID()
		if err != nil {
			return err
		}
		amounts := make(map[string]int64, len(cmd.Data))
		for resID, raw := range cmd.Data {
			if resID == "settlementId" {
				continue
			}
			amount, ok := toInt64(raw)
			if !ok {
				return fmt.Errorf("add_resources: invalid amount for %q", resID)
			}
			amounts[resID] = amount
		}
		return e.AddResources(playerID, sid, amounts)
	case "upgrade_building":
		buildingID, ok := cmd.Data["building"].(string)
		if !ok || buildingID == "" {
			return fmt.Errorf("missing building parameter")
		}
		sid, err := settlementID()
		if err != nil {
			return err
		}
		return e.UpgradeBuilding(playerID, sid, buildingID)
	case "train_unit":
		unitType, ok := cmd.Data["unit"].(string)
		if !ok || unitType == "" {
			return fmt.Errorf("missing unit parameter")
		}
		count, ok := toInt64(cmd.Data["count"])
		if !ok || count <= 0 {
			return fmt.Errorf("invalid count parameter")
		}
		sid, err := settlementID()
		if err != nil {
			return err
		}
		return e.TrainUnits(playerID, sid, unitType, int(count))
	case "finish_now":
		sid, err := settlementID()
		if err != nil {
			return err
		}
		return e.InstantComplete(playerID, sid, time.Now().Unix())
	case "get_map":
		hub.Broadcast(transport.MessageTypeMap, e.GetMap())
		return nil
	case "skip_time":
		seconds, ok := toInt64(cmd.Data["seconds"])
		if !ok || seconds <= 0 {
			return fmt.Errorf("skip_time: invalid seconds parameter")
		}
		return e.SkipTime(seconds)
	default:
		return fmt.Errorf("unknown command: %s", cmd.Name)
	}
}

// toInt64 приводит значение JSON (float64, string или int) к int64.
func toInt64(v interface{}) (int64, bool) {
	switch n := v.(type) {
	case float64:
		return int64(n), true
	case string:
		parsed, err := strconv.ParseInt(n, 10, 64)
		return parsed, err == nil
	case int:
		return int64(n), true
	case int64:
		return n, true
	default:
		return 0, false
	}
}
