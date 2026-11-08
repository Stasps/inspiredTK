// Command server - headless-сервер Inspired Kingdoms (MVP-0).
// Без клиентского транспорта: процесс грузит конфиги, восстанавливает
// снапшот, крутит тик симуляции и периодически сохраняет состояние.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"inspiredtk/server/internal/config"
	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/persistence"
	"inspiredtk/server/internal/sim"
)

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
		case <-snapTick.C:
			save()
		case <-sig:
			save()
			logger.Printf("stopped, final snapshot saved")
			return
		}
	}
}
