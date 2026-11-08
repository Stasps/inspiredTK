package persistence_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/persistence"
)

func testWorld() *domain.World {
	return &domain.World{
		Seed:        42,
		RngCounter:  7,
		IDCounter:   3,
		Tick:        123,
		CreatedUnix: 1_700_000_000,
		MapSize:     2,
		Cells: []*domain.Cell{
			{X: 0, Y: 0, Deposits: []string{"wood"}},
			{X: 1, Y: 0},
			{X: 0, Y: 1, VillageID: "v1"},
			{X: 1, Y: 1, CampID: "c1"},
		},
		Players: map[string]*domain.Player{
			"p1": {ID: "p1", Name: "P1", Windows: [2]int{10, 19}, VillageIDs: []string{"v1"}},
		},
		Villages: map[string]*domain.Village{
			"v1": {
				ID: "v1", PlayerID: "p1", X: 0, Y: 1,
				Buildings:           map[string]int{"main": 1, "sawmill": 2},
				ResourcesMilli:      map[string]int64{"wood": 510_000, "food": 999_999},
				LastSettledUnix:     1_700_003_600,
				BuildJob:            &domain.BuildJob{BuildingKey: "farm", TargetLevel: 1, FinishUnix: 1_700_010_000},
				TrainQueueRemaining: 2,
				TrainNextFinishUnix: 1_700_004_000,
				StationedInfantry:   5,
			},
		},
		Camps: map[string]*domain.Camp{
			"c1": {ID: "c1", X: 1, Y: 1, Infantry: 25, StockMilli: map[string]int64{"wood": 300_000}, NextAttackUnix: 1_700_086_400},
		},
		Marches: map[string]*domain.March{
			"m1": {
				ID: "m1", PlayerID: "p1", Type: domain.MarchAttack, Infantry: 4,
				FromX: 0, FromY: 1, ToX: 1, ToY: 1,
				OriginVillageID: "v1", TargetCampID: "c1",
				DepartUnix: 1_700_000_000, ArriveUnix: 1_700_000_600, ProvisionMilli: 40_000,
			},
		},
	}
}

func TestSnapshotRoundTrip(t *testing.T) {
	path := t.TempDir() + "/snapshot.json"
	snap := persistence.NewSnapshot(testWorld(), 1_700_005_000)
	if err := persistence.SaveSnapshot(path, snap); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := persistence.LoadSnapshot(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("marshal original: %v", err)
	}
	got, err := json.Marshal(loaded)
	if err != nil {
		t.Fatalf("marshal loaded: %v", err)
	}
	if string(got) != string(want) {
		t.Fatalf("round trip mismatch:\ngot  %s\nwant %s", got, want)
	}
}

func TestSnapshotHasVersionAndMetadata(t *testing.T) {
	path := t.TempDir() + "/snapshot.json"
	if err := persistence.SaveSnapshot(path, persistence.NewSnapshot(testWorld(), 1_700_005_000)); err != nil {
		t.Fatalf("save: %v", err)
	}
	loaded, err := persistence.LoadSnapshot(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Version != domain.SnapshotVersion {
		t.Fatalf("version: got %d", loaded.Version)
	}
	if loaded.Tick != 123 || loaded.ServerTimeUnix != 1_700_005_000 {
		t.Fatalf("metadata lost: tick %d, time %d", loaded.Tick, loaded.ServerTimeUnix)
	}
}

func TestSnapshotRejectsUnknownVersion(t *testing.T) {
	path := t.TempDir() + "/snapshot.json"
	raw := `{"version": 999, "tick": 0, "serverTimeUnix": 0, "world": {}}`
	if err := writeFile(path, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, err := persistence.LoadSnapshot(path)
	if err == nil || !strings.Contains(err.Error(), "unsupported version") {
		t.Fatalf("want version error, got %v", err)
	}
}

func TestLoadMissingSnapshotReturnsNil(t *testing.T) {
	snap, err := persistence.LoadSnapshot(t.TempDir() + "/no-such-file.json")
	if err != nil || snap != nil {
		t.Fatalf("missing snapshot must be (nil, nil), got %v, %v", snap, err)
	}
}

func TestEventLogAppendsJSONLines(t *testing.T) {
	var buf strings.Builder
	log := persistence.NewEventLog(&buf)
	ev := persistence.BattleEvent{
		Tick: 1, TimeUnix: 2, Mode: "attack", MarchID: "m1",
		AttackerID: "p1", DefenderID: "c1",
		AttackerBefore: 10, DefenderBefore: 3,
		AttackerLosses: 4, DefenderLosses: 3, AttackerWon: true,
	}
	if err := log.LogBattle(ev); err != nil {
		t.Fatalf("log: %v", err)
	}
	if err := log.LogBattle(ev); err != nil {
		t.Fatalf("log: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 JSONL lines, got %d", len(lines))
	}
	for _, line := range lines {
		var decoded persistence.BattleEvent
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("invalid JSONL line: %v", err)
		}
		if decoded.Type != "battle" {
			t.Fatalf("event type: %q", decoded.Type)
		}
	}
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o644)
}
