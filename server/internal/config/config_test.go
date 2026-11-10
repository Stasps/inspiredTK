package config

import (
	"strings"
	"testing"
)

func validConfig() *Config {
	return &Config{
		Resources: ResourcesFile{Resources: []ResourceConfig{
			{ID: "wood", Category: "nonfood"},
			{ID: "food", Category: "food"},
		}},
		Buildings: BuildingsFile{Buildings: []BuildingConfig{
			{
				ID: "sawmill", MaxLevel: 1, ProductionResource: "wood",
				Levels: []LevelConfig{{Level: 1, Cost: map[string]int64{"wood": 10}, BuildSeconds: 10, ProductionPerHour: 5}},
			},
		}},
		Units: UnitsFile{Units: []UnitConfig{
			{ID: "infantry", Attack: 10, Defense: 10, SpeedCellsPerHour: 6, Cost: map[string]int64{"wood": 100}, TrainSeconds: 30, Slots: 1},
		}},
		Combat: CombatFile{
			DefenseModifierNoWallsPerMille:   1500,
			DefenseModifierMaxWallsPerMille:  3000,
			RaidSuperiorityThresholdPerMille: 2000,
			RaidLossFactorPerMille:           500,
			ProvisionTiers: []ProvisionTier{
				{MaxDistanceCells: 10, FoodPerUnit: 10},
				{MaxDistanceCells: 0, FoodPerUnit: 50},
			},
		},
		World: WorldFile{
			MapSize:                      101,
			CampCountMin:                 5,
			CampCountMax:                 10,
			CampArmyMin:                  10,
			CampArmyMax:                  50,
			BarbarianAttackIntervalHours: 24,
			SnapshotIntervalSeconds:      300,
			SnapshotPath:                 "data/snapshot.json",
			EventLogPath:                 "data/events.jsonl",
			StartResources:               map[string]int64{"wood": 500, "food": 500},
			StorageCapacityFood:          1000,
			StorageCapacityNonFood:       1000,
			DepositChancePerMille:        map[string]int64{"wood": 400},
			WindowHours:                  3,
			WindowsPerPlayer:             2,
		},
	}
}

func TestLoadShippedConfigs(t *testing.T) {
	if _, err := Load("../../configs"); err != nil {
		t.Fatalf("shipped configs must load: %v", err)
	}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	if err := validConfig().Validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

func TestValidateDuplicateResourceID(t *testing.T) {
	cfg := validConfig()
	cfg.Resources.Resources = append(cfg.Resources.Resources, ResourceConfig{ID: "wood", Category: "food"})
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "duplicate resource id") {
		t.Fatalf("want duplicate resource id error, got %v", err)
	}
}

func TestValidateUnknownCostResource(t *testing.T) {
	cfg := validConfig()
	cfg.Buildings.Buildings[0].Levels[0].Cost = map[string]int64{"gold": 10}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "unknown resource") {
		t.Fatalf("want unknown resource error, got %v", err)
	}
}

func TestValidateBuildingLevelsSequential(t *testing.T) {
	cfg := validConfig()
	cfg.Buildings.Buildings[0].MaxLevel = 2
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "maxLevel") {
		t.Fatalf("want maxLevel mismatch error, got %v", err)
	}
}

func TestValidateProvisionTiers(t *testing.T) {
	cfg := validConfig()
	cfg.Combat.ProvisionTiers = []ProvisionTier{
		{MaxDistanceCells: 0, FoodPerUnit: 10},
		{MaxDistanceCells: 10, FoodPerUnit: 25},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "provisionTiers") {
		t.Fatalf("want provisionTiers error, got %v", err)
	}
}

func TestValidateWindowLimits(t *testing.T) {
	cfg := validConfig()
	cfg.World.WindowsPerPlayer = 9
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "windows") {
		t.Fatalf("want window limit error, got %v", err)
	}
}
