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

	// Debug-режим: при пустом мире создаём игрока-заглушку, чтобы клиенту
	// было что отображать без полноценной регистрации.
	if cfg.World.DebugMode && len(engine.World.Players) == 0 {
		if err := createDebugPlayer(engine); err != nil {
			logger.Printf("debug player: %v", err)
		} else {
			logger.Printf("debug player created: settlement of player %q", debugPlayerID)
		}
	}

	// Догон после простоя сервера: все просроченные события применяются по
	// порядку (fire_at, seq) до приёма соединений.
	if n := engine.Advance(time.Now().Unix()); n > 0 {
		logger.Printf("scheduler: caught up %d overdue events", n)
	}

	// Initialize WebSocket server; commands are consumed in the main select loop.
	wsServer := transport.NewServer()

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
	// адресной рассылкой (GAME/roadmap.md, «транспорт под модель»).
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
		case cmd := <-wsServer.CommandChan():
			// Инвариант: перед командой применены все события с fire_at <= now.
			engine.Advance(time.Now().Unix())
			if err := executeCommand(engine, wsServer, cmd); err != nil {
				logger.Printf("command %s failed: %v", cmd.Name, err)
				wsServer.Broadcast(transport.MessageTypeError, map[string]string{
					"error": fmt.Sprintf("command %s: %v", cmd.Name, err),
				})
			}
			// Команда могла поставить событие раньше прежнего ближайшего
			// (или сдвинуть время в debug-режиме).
			rearm()
		case <-pushTick.C:
			wsServer.BroadcastState(stateOf(engine))
		case <-snapTick.C:
			save()
		case <-sig:
			save()
			logger.Printf("stopped, final snapshot saved")
			return
		}
	}
}

// debugPlayerID - аккаунт-заглушка, создаваемый в debugMode при пустом мире.
const debugPlayerID = "debug"

// stateOf собирает состояние для рассылки: мир + состояние поселения debug-игрока.
func stateOf(e *sim.Engine) interface{} {
	v, err := debugSettlement(e)
	if err != nil {
		return e.GetClientState("", time.Now().Unix())
	}
	return e.GetClientState(v.ID, time.Now().Unix())
}

// createDebugPlayer сажает debug-игрока в центр карты (0,0); если центр занят -
// на ближайшую к свободную клетку с месторождением (чтобы шахта mine:<resource>
// была доступна сразу), если месторождений рядом нет - на ближайшую свободную.
func createDebugPlayer(e *sim.Engine) error {
	bestDeposit := -1
	bestFree := -1
	var bestDepositDist, bestFreeDist int64 = -1, -1
	for i, c := range e.World.Cells {
		if c.SettlementID != "" || c.CampID != "" {
			continue
		}
		dx := int64(c.X)
		dy := int64(c.Y)
		d := dx*dx + dy*dy
		if len(c.Deposits) > 0 && (bestDeposit < 0 || d < bestDepositDist) {
			bestDeposit, bestDepositDist = i, d
		}
		if bestFree < 0 || d < bestFreeDist {
			bestFree, bestFreeDist = i, d
		}
	}
	idx := bestDeposit
	if idx < 0 {
		idx = bestFree
	}
	if idx >= 0 {
		c := e.World.Cells[idx]
		return e.CreatePlayer(debugPlayerID, "Debug Player", c.X, c.Y, 10, 19, time.Now().Unix())
	}
	return fmt.Errorf("no free cell on the map")
}

// debugSettlement возвращает поселение debug-игрока (первая и единственная).
func debugSettlement(e *sim.Engine) (*domain.Settlement, error) {
	p, ok := e.World.Players[debugPlayerID]
	if !ok || len(p.SettlementIDs) == 0 {
		return nil, fmt.Errorf("no debug player settlement")
	}
	return e.World.Settlements[p.SettlementIDs[0]], nil
}

// executeCommand routes a queued client command to the engine.
// Engine is not thread-safe, so this is called only from the main loop.
func executeCommand(e *sim.Engine, ws *transport.Server, cmd transport.Command) error {
	switch cmd.Name {
	case "add_resources":
		amounts := make(map[string]int64, len(cmd.Data))
		for resID, raw := range cmd.Data {
			amount, ok := toInt64(raw)
			if !ok {
				return fmt.Errorf("add_resources: invalid amount for %q", resID)
			}
			amounts[resID] = amount
		}
		v, err := debugSettlement(e)
		if err != nil {
			return err
		}
		return e.AddResources(v.ID, amounts)
	case "upgrade_building":
		buildingID, ok := cmd.Data["building"].(string)
		if !ok || buildingID == "" {
			return fmt.Errorf("missing building parameter")
		}
		v, err := debugSettlement(e)
		if err != nil {
			return err
		}
		return e.UpgradeBuilding(v.ID, buildingID)
	case "train_unit":
		unitType, ok := cmd.Data["unit"].(string)
		if !ok || unitType == "" {
			return fmt.Errorf("missing unit parameter")
		}
		count, ok := toInt64(cmd.Data["count"])
		if !ok || count <= 0 {
			return fmt.Errorf("invalid count parameter")
		}
		v, err := debugSettlement(e)
		if err != nil {
			return err
		}
		return e.TrainUnits(v.ID, unitType, int(count))
	case "finish_now":
		v, err := debugSettlement(e)
		if err != nil {
			return err
		}
		return e.InstantComplete(debugPlayerID, v.ID, time.Now().Unix())
	case "get_map":
		ws.Broadcast(transport.MessageTypeMap, e.GetMap())
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
