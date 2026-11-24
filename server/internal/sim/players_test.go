package sim

import (
	"strings"
	"testing"
)

// Незнакомый игрок в debugMode создаётся со своим поселением; повторный
// вход возвращает то же поселение, второй игрок получает другую клетку.
func TestJoinPlayerCreatesOnceAndSeparatesPlayers(t *testing.T) {
	e := newTestEngine(t, testT0)
	e.Cfg.World.DebugMode = true

	alice, err := e.JoinPlayer("alice", testT0)
	if err != nil {
		t.Fatalf("join alice: %v", err)
	}
	if alice.PlayerID != "alice" || len(alice.Settlements) != 1 {
		t.Fatalf("alice info %+v, want one settlement", alice)
	}
	again, err := e.JoinPlayer("alice", testT0+10)
	if err != nil {
		t.Fatalf("rejoin alice: %v", err)
	}
	if len(again.Settlements) != 1 || again.Settlements[0] != alice.Settlements[0] {
		t.Fatalf("rejoin gave %+v, want %+v", again.Settlements, alice.Settlements)
	}
	if again.ServerTimeUnix != testT0+10 {
		t.Fatalf("server time %d", again.ServerTimeUnix)
	}

	bob, err := e.JoinPlayer("bob", testT0)
	if err != nil {
		t.Fatalf("join bob: %v", err)
	}
	a, b := alice.Settlements[0], bob.Settlements[0]
	if a.ID == b.ID || (a.X == b.X && a.Y == b.Y) {
		t.Fatalf("alice %+v and bob %+v share a settlement or a cell", a, b)
	}
	if v := e.World.Settlements[b.ID]; v.PlayerID != "bob" {
		t.Fatalf("bob's settlement owned by %q", v.PlayerID)
	}
	if c := e.World.CellAt(b.X, b.Y); c.SettlementID != b.ID {
		t.Fatalf("cell (%d,%d) holds %q, want %q", b.X, b.Y, c.SettlementID, b.ID)
	}
	if len(e.World.Players) != 2 {
		t.Fatalf("players %d, want 2", len(e.World.Players))
	}
}

// Без debugMode регистрации нет: входит только существующий игрок.
func TestJoinPlayerWithoutDebugMode(t *testing.T) {
	e := newTestEngine(t, testT0)
	e.Cfg.World.DebugMode = false
	newPlayerSettlement(t, e, "p1", 10, 10, testT0)

	if _, err := e.JoinPlayer("p1", testT0); err != nil {
		t.Fatalf("existing player: %v", err)
	}
	if _, err := e.JoinPlayer("stranger", testT0); err == nil || !strings.Contains(err.Error(), "registration") {
		t.Fatalf("unknown player: err %v, want registration error", err)
	}
	if _, ok := e.World.Players["stranger"]; ok {
		t.Fatal("unknown player was created without debugMode")
	}
	if _, err := e.JoinPlayer("", testT0); err == nil {
		t.Fatal("empty player id accepted")
	}
}

// Команды на чужое поселение отклоняются и ничего не меняют.
func TestCommandsRejectForeignSettlement(t *testing.T) {
	e := newTestEngine(t, testT0)
	e.Cfg.World.DebugMode = true
	mine := newPlayerSettlement(t, e, "p1", 10, 10, testT0)
	foreign := newPlayerSettlement(t, e, "p2", -10, -10, testT0)
	woodBefore := foreign.ResourcesMilli["wood"]

	if err := e.AddResources("p1", foreign.ID, map[string]int64{"wood": 100}); err == nil {
		t.Fatal("add_resources to a foreign settlement accepted")
	}
	if err := e.UpgradeBuilding("p1", foreign.ID, "main"); err == nil {
		t.Fatal("upgrade_building in a foreign settlement accepted")
	}
	if err := e.TrainUnits("p1", foreign.ID, "infantry", 1); err == nil {
		t.Fatal("train_unit in a foreign settlement accepted")
	}
	if err := e.InstantComplete("p1", foreign.ID, testT0); err == nil {
		t.Fatal("finish_now in a foreign settlement accepted")
	}
	if foreign.BuildJob != nil || foreign.TrainQueueRemaining != 0 || foreign.ResourcesMilli["wood"] != woodBefore {
		t.Fatalf("foreign settlement changed: %+v", foreign)
	}

	if err := e.AddResources("p1", mine.ID, map[string]int64{"wood": 100}); err != nil {
		t.Fatalf("add_resources to own settlement: %v", err)
	}
	if err := e.UpgradeBuilding("p1", mine.ID, "main"); err != nil {
		t.Fatalf("upgrade_building in own settlement: %v", err)
	}
}

// Состояние игрока подробно описывает его поселение, а не чужое.
func TestPlayerStateUsesOwnSettlement(t *testing.T) {
	e := newTestEngine(t, testT0)
	newPlayerSettlement(t, e, "p1", 10, 10, testT0)
	v2 := newPlayerSettlement(t, e, "p2", -10, -10, testT0)
	v2.StationedInfantry = 7

	st, ok := e.PlayerState("p2", testT0).(clientState)
	if !ok || st.Settlement == nil {
		t.Fatalf("p2 state has no settlement: %#v", st)
	}
	if st.Settlement.StationedInfantry != 7 {
		t.Fatalf("p2 state shows %d infantry, want 7 (own settlement)", st.Settlement.StationedInfantry)
	}
	if st, _ := e.PlayerState("nobody", testT0).(clientState); st.Settlement != nil {
		t.Fatal("unknown player got a settlement state")
	}
}
