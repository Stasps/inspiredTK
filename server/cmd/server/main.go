// Command server - headless-сервер Inspired Kingdoms (MVP-0).
// Без клиентского транспорта: процесс грузит конфиги, восстанавливает
// снапшот, крутит тик симуляции и периодически сохраняет состояние.
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
		logger.Printf("snapshot restored: tick %d, server time %d", snap.Tick, snap.ServerTimeUnix)
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
			logger.Printf("debug player created: village of player %q", debugPlayerID)
		}
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

	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	snapTick := time.NewTicker(time.Duration(cfg.World.SnapshotIntervalSeconds) * time.Second)
	defer snapTick.Stop()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)

	logger.Printf("started: tick 1s, snapshot every %ds", cfg.World.SnapshotIntervalSeconds)
	for {
		select {
		case t := <-tick.C:
			engine.Tick(t.Unix())
			wsServer.BroadcastState(stateOf(engine))
		case cmd := <-wsServer.CommandChan():
			if err := executeCommand(engine, wsServer, cmd); err != nil {
				logger.Printf("command %s failed: %v", cmd.Name, err)
				wsServer.Broadcast(transport.MessageTypeError, map[string]string{
					"error": fmt.Sprintf("command %s: %v", cmd.Name, err),
				})
			}
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

// stateOf собирает состояние для рассылки: мир + состояние деревни debug-игрока.
func stateOf(e *sim.Engine) interface{} {
	v, err := debugVillage(e)
	if err != nil {
		return e.GetClientState("", time.Now().Unix())
	}
	return e.GetClientState(v.ID, time.Now().Unix())
}

// createDebugPlayer сажает debug-игрока на первую свободную клетку карты
// с месторождением, чтобы шахта (mine:<resource>) была доступна сразу;
// если месторождений рядом нет - на первую свободную клетку.
func createDebugPlayer(e *sim.Engine) error {
	fallback := -1
	for i, c := range e.World.Cells {
		if c.VillageID != "" || c.CampID != "" {
			continue
		}
		if fallback < 0 {
			fallback = i
		}
		if len(c.Deposits) > 0 {
			return e.CreatePlayer(debugPlayerID, "Debug Player", c.X, c.Y, 10, 19, time.Now().Unix())
		}
	}
	if fallback >= 0 {
		c := e.World.Cells[fallback]
		return e.CreatePlayer(debugPlayerID, "Debug Player", c.X, c.Y, 10, 19, time.Now().Unix())
	}
	return fmt.Errorf("no free cell on the map")
}

// debugVillage возвращает деревню debug-игрока (первая и единственная).
func debugVillage(e *sim.Engine) (*domain.Village, error) {
	p, ok := e.World.Players[debugPlayerID]
	if !ok || len(p.VillageIDs) == 0 {
		return nil, fmt.Errorf("no debug player village")
	}
	return e.World.Villages[p.VillageIDs[0]], nil
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
		v, err := debugVillage(e)
		if err != nil {
			return err
		}
		return e.AddResources(v.ID, amounts)
	case "upgrade_building":
		buildingID, ok := cmd.Data["building"].(string)
		if !ok || buildingID == "" {
			return fmt.Errorf("missing building parameter")
		}
		v, err := debugVillage(e)
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
		v, err := debugVillage(e)
		if err != nil {
			return err
		}
		return e.TrainUnits(v.ID, unitType, int(count))
	case "finish_now":
		v, err := debugVillage(e)
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
