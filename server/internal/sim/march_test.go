package sim

import (
	"fmt"
	"testing"

	"inspiredtk/server/internal/domain"
)

// placeCamp ставит тестовый лагерь на клетку, вытесняя сгенерированный.
func placeCamp(t *testing.T, e *Engine, x, y, army int) *domain.Camp {
	t.Helper()
	cell := e.World.CellAt(x, y)
	if cell == nil {
		t.Fatalf("cell (%d,%d) outside map", x, y)
	}
	if cell.VillageID != "" {
		t.Fatalf("cell (%d,%d) occupied by village", x, y)
	}
	if cell.CampID != "" {
		delete(e.World.Camps, cell.CampID)
	}
	camp := &domain.Camp{
		ID:         fmt.Sprintf("tc-%d-%d", x, y),
		X:          x,
		Y:          y,
		Infantry:   army,
		StockMilli: map[string]int64{"wood": 300_000},
	}
	cell.CampID = camp.ID
	e.World.Camps[camp.ID] = camp
	return camp
}

// prepareWarVillage - деревня с 20 пехотинцами и запасом еды.
func prepareWarVillage(t *testing.T, e *Engine, x, y int, now int64) *domain.Village {
	t.Helper()
	v := newPlayerVillage(t, e, "p1", x, y, now)
	v.StationedInfantry = 20
	v.ResourcesMilli["food"] = 2_000_000
	return v
}

func TestMarchProvisionTiersAndTravelTime(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := prepareWarVillage(t, e, 50, 50, testT0)

	cases := []struct {
		name        string
		x, y        int
		wantPerUnit int64
		wantTravel  int64
	}{
		{"exactly 10 cells", 56, 58, 10, 6000},          // sqrt(36+64) = 10
		{"exactly 30 cells", 50, 80, 25, 18000},         // ровно 30 клеток
		{"31 cells", 50, 81, 50, 18600},                 // свыше 30
		{"10.6 cells rounds to tier 25", 43, 58, 25, 0}, // sqrt(49+64) ~ 10.63
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			placeCamp(t, e, tc.x, tc.y, 1)
			foodBefore := v.ResourcesMilli["food"]
			id, err := e.SendMarch("p1", v.ID, domain.MarchAttack, tc.x, tc.y, 1, testT0)
			if err != nil {
				t.Fatalf("send march: %v", err)
			}
			m := e.World.Marches[id]
			wantProvision := tc.wantPerUnit * 1_000
			if m.ProvisionMilli != wantProvision {
				t.Fatalf("provision: got %d, want %d", m.ProvisionMilli, wantProvision)
			}
			if got := foodBefore - v.ResourcesMilli["food"]; got != wantProvision {
				t.Fatalf("food deducted: got %d, want %d", got, wantProvision)
			}
			if tc.wantTravel > 0 {
				if got := m.ArriveUnix - m.DepartUnix; got != tc.wantTravel {
					t.Fatalf("travel seconds: got %d, want %d", got, tc.wantTravel)
				}
			}
			delete(e.World.Marches, id)
			v.StationedInfantry = 20
			v.ResourcesMilli["food"] = 2_000_000
		})
	}
}

func TestCancelMarchRefundsProvisionAndReturnsTroops(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := prepareWarVillage(t, e, 50, 50, testT0)
	placeCamp(t, e, 56, 58, 1)

	id, err := e.SendMarch("p1", v.ID, domain.MarchAttack, 56, 58, 5, testT0)
	if err != nil {
		t.Fatalf("send march: %v", err)
	}
	if v.StationedInfantry != 15 {
		t.Fatalf("stationed after send: %d", v.StationedInfantry)
	}
	foodBefore := v.ResourcesMilli["food"]
	if err := e.CancelMarch("p1", id, testT0+100); err != nil {
		t.Fatalf("cancel march: %v", err)
	}
	if _, ok := e.World.Marches[id]; ok {
		t.Fatalf("cancelled march still present")
	}
	if v.ResourcesMilli["food"] != foodBefore+50_000 {
		t.Fatalf("provision not refunded: food %d", v.ResourcesMilli["food"])
	}
	// Обратный марш: дойдёт за столько же, сколько прошёл до отмены.
	if len(e.World.Marches) != 1 {
		t.Fatalf("want 1 return march, got %d", len(e.World.Marches))
	}
	e.Tick(testT0 + 200)
	if v.StationedInfantry != 20 {
		t.Fatalf("troops did not return: %d", v.StationedInfantry)
	}
}

func TestCampBattleVictoryLootAndReturn(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := prepareWarVillage(t, e, 50, 50, testT0)
	camp := placeCamp(t, e, 56, 58, 3)

	id, err := e.SendMarch("p1", v.ID, domain.MarchAttack, 56, 58, 10, testT0)
	if err != nil {
		t.Fatalf("send march: %v", err)
	}
	arrive := e.World.Marches[id].ArriveUnix
	e.Tick(arrive)
	// Атака 10x10=100 vs защита 3x10x1.5=45: лагерь уничтожен,
	// потери атакующего floor(10*45/100) = 4.
	if camp.Infantry != 0 {
		t.Fatalf("camp must be wiped: %d", camp.Infantry)
	}
	if len(camp.StockMilli) != 0 {
		t.Fatalf("camp stock must be looted: %v", camp.StockMilli)
	}
	if len(e.World.Marches) != 1 {
		t.Fatalf("want return march with survivors, got %d marches", len(e.World.Marches))
	}
	for _, m := range e.World.Marches {
		if m.Type != domain.MarchReturn || m.Infantry != 6 {
			t.Fatalf("return march: %+v", m)
		}
		e.Tick(m.ArriveUnix)
	}
	if v.StationedInfantry != 16 {
		t.Fatalf("survivors must return home: %d", v.StationedInfantry)
	}
	if v.ResourcesMilli["wood"] != 500_000+300_000 {
		t.Fatalf("loot must reach village storage: %d", v.ResourcesMilli["wood"])
	}
}

func TestMarchFailsWithoutTroopsOrFood(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerVillage(t, e, "p1", 50, 50, testT0)
	placeCamp(t, e, 56, 58, 1)
	if _, err := e.SendMarch("p1", v.ID, domain.MarchAttack, 56, 58, 1, testT0); err == nil {
		t.Fatalf("march without troops must fail")
	}
	v.StationedInfantry = 5
	v.ResourcesMilli["food"] = 0
	if _, err := e.SendMarch("p1", v.ID, domain.MarchAttack, 56, 58, 1, testT0); err == nil {
		t.Fatalf("march without provisions must fail")
	}
	if v.StationedInfantry != 5 {
		t.Fatalf("failed march must not move troops")
	}
}

func TestSimultaneousSpendsDoNotDoubleCharge(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerVillage(t, e, "p1", 50, 50, testT0)
	v.Buildings = map[string]int{"main": 1, "barracks": 1}
	// Дерева хватает либо на тренировку (100), либо на лесопилку (40), но не на дюп:
	// ровно 100 дерева, железа на одного пехотинца.
	v.ResourcesMilli = map[string]int64{"wood": 100_000, "stone": 10_000, "iron": 50_000, "food": 500_000}

	if err := e.StartTraining("p1", v.ID, 1, testT0); err != nil {
		t.Fatalf("training: %v", err)
	}
	// Лесопилка больше не должна построиться: дерево уже потрачено.
	if err := e.StartConstruction("p1", v.ID, "sawmill", testT0); err == nil {
		t.Fatalf("construction must fail: wood already spent on training")
	}
	if v.ResourcesMilli["wood"] != 0 {
		t.Fatalf("wood charged twice: %d", v.ResourcesMilli["wood"])
	}
	if v.BuildJob != nil {
		t.Fatalf("failed construction left a build job")
	}
}
