package sim

import (
	"io"
	"testing"

	"inspiredtk/server/internal/config"
	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/persistence"
)

// testT0 - фиксированное «серверное» время для детерминированных тестов.
const testT0 = int64(1_700_000_000)

func newTestEngine(t *testing.T, now int64) *Engine {
	t.Helper()
	cfg, err := config.Load("../../configs")
	if err != nil {
		t.Fatalf("load configs: %v", err)
	}
	// Базовая выработка отключена в тестах: тесты механик не должны зависеть
	// от прохода серверного времени (её проверяет TestBaseProduction).
	cfg.World.BaseProductionPerHour = 0
	return NewEngine(cfg, NewWorld(cfg, now), persistence.NewEventLog(io.Discard))
}

// newPlayerVillage создаёт игрока с деревней и возвращает её.
func newPlayerVillage(t *testing.T, e *Engine, playerID string, x, y int, now int64) *domain.Village {
	t.Helper()
	if err := e.CreatePlayer(playerID, "Player "+playerID, x, y, 10, 19, now); err != nil {
		t.Fatalf("create player: %v", err)
	}
	p := e.World.Players[playerID]
	return e.World.Villages[p.VillageIDs[0]]
}

func TestProductionAccruesByElapsedTime(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerVillage(t, e, "p1", 10, 10, testT0)
	if err := e.StartConstruction("p1", v.ID, "sawmill", testT0); err != nil {
		t.Fatalf("start construction: %v", err)
	}
	if err := e.InstantComplete("p1", v.ID, testT0); err != nil {
		t.Fatalf("instant complete: %v", err)
	}
	woodBefore := v.ResourcesMilli["wood"]
	e.settle(v, testT0+3600)
	// Лесопилка 1 ур.: 10 дерева/час = 10000 милли-единиц за час.
	want := woodBefore + 10_000
	if got := v.ResourcesMilli["wood"]; got != want {
		t.Fatalf("wood after 1h: got %d, want %d", got, want)
	}
	if v.LastSettledUnix != testT0+3600 {
		t.Fatalf("lastSettledUnix: got %d", v.LastSettledUnix)
	}
}

func TestProductionCappedAtStorage(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerVillage(t, e, "p1", 10, 10, testT0)
	v.Buildings = map[string]int{"main": 1, "farm": 5}
	// Почти полный продовольственный склад (вместимость 1000 ед.): свободно
	// ровно 3 единицы.
	for resID := range v.ResourcesMilli {
		v.ResourcesMilli[resID] = 0
	}
	v.ResourcesMilli["food"] = 1_000_000 - 3_000
	e.settle(v, testT0+10*3600)
	if got := v.ResourcesMilli["food"]; got != 1_000_000 {
		t.Fatalf("food must cap at storage: got %d, want 1000000", got)
	}
}

// TestBaseProduction проверяет базовую выработку каждой деревни по конфигу
// мира (baseProductionPerHour на каждый ресурс, без зданий).
func TestBaseProduction(t *testing.T) {
	e := newTestEngine(t, testT0)
	e.Cfg.World.BaseProductionPerHour = 10
	v := newPlayerVillage(t, e, "p1", 10, 10, testT0)
	e.settle(v, testT0+3600)
	for _, resID := range []string{"wood", "stone", "iron", "food"} {
		// Старт 500 ед. + 10 ед./час = 510 ед.
		if got := v.ResourcesMilli[resID]; got != 510_000 {
			t.Fatalf("%s after 1h: got %d, want 510000", resID, got)
		}
	}
}

func TestSettleIdempotent(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerVillage(t, e, "p1", 10, 10, testT0)
	v.Buildings = map[string]int{"main": 1, "sawmill": 2, "farm": 1}
	e.settle(v, testT0+7200)
	snapshot := make(map[string]int64, len(v.ResourcesMilli))
	for k, val := range v.ResourcesMilli {
		snapshot[k] = val
	}
	e.settle(v, testT0+7200)
	for k, val := range snapshot {
		if v.ResourcesMilli[k] != val {
			t.Fatalf("second settle at same time changed %s: %d -> %d", k, val, v.ResourcesMilli[k])
		}
	}
}

func TestSpendAtomicOnInsufficientFunds(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerVillage(t, e, "p1", 10, 10, testT0)
	v.Buildings = map[string]int{"main": 1, "barracks": 1}
	// Ровно на одного пехотинца: 100 дерева + 50 железа.
	v.ResourcesMilli = map[string]int64{"wood": 100_000, "stone": 0, "iron": 50_000, "food": 0}

	if err := e.StartTraining("p1", v.ID, 1, testT0); err != nil {
		t.Fatalf("first training must succeed: %v", err)
	}
	if v.ResourcesMilli["wood"] != 0 || v.ResourcesMilli["iron"] != 0 {
		t.Fatalf("resources after training: %v", v.ResourcesMilli)
	}
	before := map[string]int64{}
	for k, val := range v.ResourcesMilli {
		before[k] = val
	}
	if err := e.StartTraining("p1", v.ID, 1, testT0); err == nil {
		t.Fatalf("second training must fail: no resources")
	}
	for k, val := range before {
		if v.ResourcesMilli[k] != val {
			t.Fatalf("failed spend changed %s: %d -> %d", k, val, v.ResourcesMilli[k])
		}
	}
	if v.TrainQueueRemaining != 1 {
		t.Fatalf("train queue corrupted: %d", v.TrainQueueRemaining)
	}
}

func TestConstructionPaysOnlyOnce(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerVillage(t, e, "p1", 10, 10, testT0)
	if err := e.StartConstruction("p1", v.ID, "sawmill", testT0); err != nil {
		t.Fatalf("start construction: %v", err)
	}
	woodAfterPay := v.ResourcesMilli["wood"]
	if err := e.StartConstruction("p1", v.ID, "sawmill", testT0); err == nil {
		t.Fatalf("busy queue must reject second construction")
	}
	if v.ResourcesMilli["wood"] != woodAfterPay {
		t.Fatalf("rejected construction charged resources")
	}
	e.Tick(v.BuildJob.FinishUnix)
	if v.Buildings["sawmill"] != 1 || v.BuildJob != nil {
		t.Fatalf("construction did not finish: %+v", v.BuildJob)
	}
}

func TestInstantCompleteRequiresDebugMode(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerVillage(t, e, "p1", 10, 10, testT0)
	e.Cfg.World.DebugMode = false
	if err := e.InstantComplete("p1", v.ID, testT0); err == nil {
		t.Fatalf("instant complete must be rejected without debugMode")
	}
}
