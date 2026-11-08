package sim

import (
	"testing"
	"time"

	"inspiredtk/server/internal/domain"
)

func testBattleParams() BattleParams {
	return BattleParams{
		AttackStat:              10,
		DefenseStat:             10,
		DefenseModifierPerMille: 1500,
		RaidThresholdPerMille:   2000,
		RaidFactorPerMille:      500,
	}
}

func TestBattleDeterministic(t *testing.T) {
	p := testBattleParams()
	first := ResolveBattle(37, 11, domain.MarchRaid, p)
	for i := 0; i < 100; i++ {
		if got := ResolveBattle(37, 11, domain.MarchRaid, p); got != first {
			t.Fatalf("non-deterministic battle: %+v vs %+v", got, first)
		}
	}
}

func TestAttackLoserWipedOut(t *testing.T) {
	// Атака 100x10=1000 vs защита 10x10x1.5=150: защитник теряет всё,
	// атакующий floor(100*150/1000) = 15.
	out := ResolveBattle(100, 10, domain.MarchAttack, testBattleParams())
	if !out.AttackerWon || out.DefenderLosses != 10 || out.AttackerLosses != 15 {
		t.Fatalf("unexpected outcome: %+v", out)
	}
}

func TestAttackDefenderWins(t *testing.T) {
	// Атака 10x10=100 vs защита 100x10x1.5=1500: атакующий теряет всё,
	// защитник floor(100*100/1500) = 6.
	out := ResolveBattle(10, 100, domain.MarchAttack, testBattleParams())
	if out.AttackerWon || out.AttackerLosses != 10 || out.DefenderLosses != 6 {
		t.Fatalf("unexpected outcome: %+v", out)
	}
}

func TestDefenseModifierApplies(t *testing.T) {
	// Без модификатора 67 защитников слабее 100 атакующих (670 < 1000),
	// с модификатором 1.5 - сильнее (1005 > 1000).
	out := ResolveBattle(100, 67, domain.MarchAttack, testBattleParams())
	if out.AttackerWon {
		t.Fatalf("defense modifier 1.5 must flip the battle: %+v", out)
	}
}

func TestRaidWithSuperiorityWipesLoser(t *testing.T) {
	// Перевес 1000/150 >= 2.0: набег ведёт себя как атака.
	out := ResolveBattle(100, 10, domain.MarchRaid, testBattleParams())
	if !out.AttackerWon || out.DefenderLosses != 10 {
		t.Fatalf("raid with superiority must wipe defender: %+v", out)
	}
}

func TestRaidWithoutSuperioritySharesLosses(t *testing.T) {
	// Атака 20x10=200 vs защита 10x10x1.5=150: перевес 1.33 < 2.0.
	// Потери атакующего floor(20*150*500/(200*1000)) = 7,
	// защитника floor(10*200*500/(150*1000)) = 6.
	out := ResolveBattle(20, 10, domain.MarchRaid, testBattleParams())
	if !out.AttackerWon || out.AttackerLosses != 7 || out.DefenderLosses != 6 {
		t.Fatalf("unexpected raid outcome: %+v", out)
	}
	if out.AttackerLosses == 20 || out.DefenderLosses == 10 {
		t.Fatalf("raid without superiority must not wipe any side: %+v", out)
	}
}

func TestBattleNoDefenders(t *testing.T) {
	out := ResolveBattle(5, 0, domain.MarchAttack, testBattleParams())
	if !out.AttackerWon || out.AttackerLosses != 0 {
		t.Fatalf("empty target must be a free win: %+v", out)
	}
}

func TestBarbarianAttacksOnlyInsidePlayerWindows(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Unix()
	e := newTestEngine(t, base)
	v := newPlayerVillage(t, e, "p1", 50, 50, base)
	if err := e.SetWindows("p1", 10, 14); err != nil {
		t.Fatalf("set windows: %v", err)
	}

	var camp *domain.Camp
	for _, c := range e.World.Camps {
		camp = c
		break
	}
	if camp == nil {
		t.Fatalf("no camps generated")
	}
	army := camp.Infantry
	camp.NextAttackUnix = base

	e.Tick(base)
	if camp.TargetVillageID != v.ID {
		t.Fatalf("camp must target the only village: %q", camp.TargetVillageID)
	}
	wantDepart := base + 10*3600
	if camp.ScheduledDepartUnix != wantDepart {
		t.Fatalf("depart must wait for the window: got %d, want %d", camp.ScheduledDepartUnix, wantDepart)
	}
	departHour := time.Unix(camp.ScheduledDepartUnix, 0).UTC().Hour()
	if departHour < 10 || departHour >= 13 {
		t.Fatalf("depart hour %d outside window [10,13)", departHour)
	}

	e.Tick(camp.ScheduledDepartUnix)
	if camp.Infantry != 0 {
		t.Fatalf("camp army must depart: %d", camp.Infantry)
	}
	var march *domain.March
	for _, m := range e.World.Marches {
		if m.OriginCampID == camp.ID {
			march = m
		}
	}
	if march == nil || march.Infantry != army || march.TargetVillageID != v.ID {
		t.Fatalf("barbarian march: %+v", march)
	}
	if camp.NextAttackUnix-camp.ScheduledDepartUnix != 24*3600 {
		t.Fatalf("next attack must be scheduled 24h later: %d", camp.NextAttackUnix-camp.ScheduledDepartUnix)
	}
}
