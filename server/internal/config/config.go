// Package config - слой Data: загрузка и валидация файлов конфигов игры.
// Контент игры живёт только здесь, в коде он не хардкодится (см. GAME/architecture.md, «НЕЛЬЗЯ»).
package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ResourceConfig - конфиг одного ресурса.
type ResourceConfig struct {
	ID       string `json:"id"`
	Category string `json:"category"` // "food" или "nonfood"
}

// ResourcesFile - файл resources.json.
type ResourcesFile struct {
	Resources []ResourceConfig `json:"resources"`
}

// LevelConfig - конфиг одного уровня здания (cost - в целых единицах ресурсов).
type LevelConfig struct {
	Level               int              `json:"level"`
	Cost                map[string]int64 `json:"cost"`
	BuildSeconds        int64            `json:"buildSeconds"`
	ProductionPerHour   int64            `json:"productionPerHour,omitempty"`
	BuildFactorPerMille int64            `json:"buildFactorPerMille,omitempty"`
	Slots               int              `json:"slots,omitempty"`
}

// BuildingConfig - конфиг одного типа здания.
type BuildingConfig struct {
	ID                 string        `json:"id"`
	MaxLevel           int           `json:"maxLevel"`
	ProductionResource string        `json:"productionResource,omitempty"`
	IsMine             bool          `json:"isMine,omitempty"`
	Levels             []LevelConfig `json:"levels"`
}

// BuildingsFile - файл buildings.json.
type BuildingsFile struct {
	BalanceNote string           `json:"balanceNote"`
	Buildings   []BuildingConfig `json:"buildings"`
}

// UnitConfig - конфиг одного типа юнита (cost - в целых единицах ресурсов).
type UnitConfig struct {
	ID                string           `json:"id"`
	Attack            int64            `json:"attack"`
	Defense           int64            `json:"defense"`
	SpeedCellsPerHour int64            `json:"speedCellsPerHour"`
	Cost              map[string]int64 `json:"cost"`
	TrainSeconds      int64            `json:"trainSeconds"`
	Slots             int              `json:"slots"`
}

// UnitsFile - файл units.json.
type UnitsFile struct {
	Units []UnitConfig `json:"units"`
}

// ProvisionTier - тариф провианта: MaxDistanceCells 0 означает «без верхней границы».
type ProvisionTier struct {
	MaxDistanceCells int64 `json:"maxDistanceCells"`
	FoodPerUnit      int64 `json:"foodPerUnit"`
}

// CombatFile - файл combat.json.
type CombatFile struct {
	BalanceNote                      string          `json:"balanceNote"`
	DefenseModifierNoWallsPerMille   int64           `json:"defenseModifierNoWallsPerMille"`
	DefenseModifierMaxWallsPerMille  int64           `json:"defenseModifierMaxWallsPerMille"`
	RaidSuperiorityThresholdPerMille int64           `json:"raidSuperiorityThresholdPerMille"`
	RaidLossFactorPerMille           int64           `json:"raidLossFactorPerMille"`
	ProvisionTiers                   []ProvisionTier `json:"provisionTiers"`
}

// WorldFile - файл world.json.
type WorldFile struct {
	BalanceNote                  string           `json:"balanceNote"`
	MapSize                      int              `json:"mapSize"`
	Seed                         int64            `json:"seed"`
	CampCountMin                 int              `json:"campCountMin"`
	CampCountMax                 int              `json:"campCountMax"`
	CampArmyMin                  int              `json:"campArmyMin"`
	CampArmyMax                  int              `json:"campArmyMax"`
	BarbarianAttackIntervalHours int64            `json:"barbarianAttackIntervalHours"`
	SnapshotIntervalSeconds      int64            `json:"snapshotIntervalSeconds"`
	SnapshotPath                 string           `json:"snapshotPath"`
	EventLogPath                 string           `json:"eventLogPath"`
	DebugMode                    bool             `json:"debugMode"`
	BaseProductionPerHour        int64            `json:"baseProductionPerHour"`
	StartResources               map[string]int64 `json:"startResources"`
	StorageCapacityFood          int64            `json:"storageCapacityFood"`
	StorageCapacityNonFood       int64            `json:"storageCapacityNonFood"`
	CampRewardStock              map[string]int64 `json:"campRewardStock"`
	DepositChancePerMille        map[string]int64 `json:"depositChancePerMille"`
	WindowHours                  int64            `json:"windowHours"`
	WindowsPerPlayer             int              `json:"windowsPerPlayer"`
}

// Config - все конфиги игры с индексами по ID.
type Config struct {
	Resources ResourcesFile
	Buildings BuildingsFile
	Units     UnitsFile
	Combat    CombatFile
	World     WorldFile

	ResourceByID map[string]ResourceConfig
	BuildingByID map[string]BuildingConfig
	UnitByID     map[string]UnitConfig
}

// Load читает все конфиги из каталога и валидирует их.
// Parameters:
//   - dir: каталог с файлами resources.json, buildings.json, units.json, combat.json, world.json
//
// Returns: собранный конфиг или ошибка с именем файла.
func Load(dir string) (*Config, error) {
	cfg := &Config{}
	files := []struct {
		name string
		dst  any
	}{
		{"resources.json", &cfg.Resources},
		{"buildings.json", &cfg.Buildings},
		{"units.json", &cfg.Units},
		{"combat.json", &cfg.Combat},
		{"world.json", &cfg.World},
	}
	for _, f := range files {
		if err := decodeFile(filepath.Join(dir, f.name), f.dst); err != nil {
			return nil, err
		}
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func decodeFile(path string, dst any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	return nil
}

// Validate проверяет конфиги: уникальные ID, диапазоны, ссылки между файлами.
// Ошибка всегда содержит имя файла и идентификатор сущности.
func (c *Config) Validate() error {
	if err := c.validateResources(); err != nil {
		return err
	}
	c.buildIndexes()
	if err := c.validateBuildings(); err != nil {
		return err
	}
	if err := c.validateUnits(); err != nil {
		return err
	}
	if err := c.validateCombat(); err != nil {
		return err
	}
	return c.validateWorld()
}

func (c *Config) buildIndexes() {
	c.ResourceByID = make(map[string]ResourceConfig, len(c.Resources.Resources))
	for _, r := range c.Resources.Resources {
		c.ResourceByID[r.ID] = r
	}
	c.BuildingByID = make(map[string]BuildingConfig, len(c.Buildings.Buildings))
	for _, b := range c.Buildings.Buildings {
		c.BuildingByID[b.ID] = b
	}
	c.UnitByID = make(map[string]UnitConfig, len(c.Units.Units))
	for _, u := range c.Units.Units {
		c.UnitByID[u.ID] = u
	}
}

func (c *Config) validateResources() error {
	if len(c.Resources.Resources) == 0 {
		return fmt.Errorf("resources.json: empty resources list")
	}
	seen := make(map[string]bool, len(c.Resources.Resources))
	for _, r := range c.Resources.Resources {
		if r.ID == "" {
			return fmt.Errorf("resources.json: resource with empty id")
		}
		if seen[r.ID] {
			return fmt.Errorf("resources.json: duplicate resource id %q", r.ID)
		}
		seen[r.ID] = true
		if r.Category != "food" && r.Category != "nonfood" {
			return fmt.Errorf("resources.json: resource %q: unknown category %q", r.ID, r.Category)
		}
	}
	return nil
}

func (c *Config) validateCost(file, id string, cost map[string]int64) error {
	for resID, amount := range cost {
		if _, ok := c.ResourceByID[resID]; !ok {
			return fmt.Errorf("%s: %s: cost references unknown resource %q", file, id, resID)
		}
		if amount < 0 {
			return fmt.Errorf("%s: %s: negative cost of %q", file, id, resID)
		}
	}
	return nil
}

func (c *Config) validateBuildings() error {
	if len(c.Buildings.Buildings) == 0 {
		return fmt.Errorf("buildings.json: empty buildings list")
	}
	seen := make(map[string]bool, len(c.Buildings.Buildings))
	for _, b := range c.Buildings.Buildings {
		if b.ID == "" {
			return fmt.Errorf("buildings.json: building with empty id")
		}
		if seen[b.ID] {
			return fmt.Errorf("buildings.json: duplicate building id %q", b.ID)
		}
		seen[b.ID] = true
		if b.MaxLevel <= 0 || len(b.Levels) != b.MaxLevel {
			return fmt.Errorf("buildings.json: building %q: maxLevel %d does not match %d levels", b.ID, b.MaxLevel, len(b.Levels))
		}
		if b.ProductionResource != "" {
			if _, ok := c.ResourceByID[b.ProductionResource]; !ok {
				return fmt.Errorf("buildings.json: building %q: unknown production resource %q", b.ID, b.ProductionResource)
			}
			if b.IsMine {
				return fmt.Errorf("buildings.json: building %q: mine must not fix productionResource (it comes from the deposit)", b.ID)
			}
		}
		for i, lvl := range b.Levels {
			if lvl.Level != i+1 {
				return fmt.Errorf("buildings.json: building %q: levels must be sequential, got %d at index %d", b.ID, lvl.Level, i)
			}
			if lvl.BuildSeconds <= 0 {
				return fmt.Errorf("buildings.json: building %q level %d: buildSeconds must be positive", b.ID, lvl.Level)
			}
			if lvl.ProductionPerHour < 0 {
				return fmt.Errorf("buildings.json: building %q level %d: negative productionPerHour", b.ID, lvl.Level)
			}
			if lvl.BuildFactorPerMille < 0 || lvl.Slots < 0 {
				return fmt.Errorf("buildings.json: building %q level %d: negative buildFactorPerMille or slots", b.ID, lvl.Level)
			}
			if err := c.validateCost("buildings.json", b.ID, lvl.Cost); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Config) validateUnits() error {
	if len(c.Units.Units) == 0 {
		return fmt.Errorf("units.json: empty units list")
	}
	seen := make(map[string]bool, len(c.Units.Units))
	for _, u := range c.Units.Units {
		if u.ID == "" {
			return fmt.Errorf("units.json: unit with empty id")
		}
		if seen[u.ID] {
			return fmt.Errorf("units.json: duplicate unit id %q", u.ID)
		}
		seen[u.ID] = true
		if u.Attack <= 0 || u.Defense <= 0 {
			return fmt.Errorf("units.json: unit %q: attack and defense must be positive", u.ID)
		}
		if u.SpeedCellsPerHour <= 0 || u.TrainSeconds <= 0 || u.Slots <= 0 {
			return fmt.Errorf("units.json: unit %q: speed, trainSeconds and slots must be positive", u.ID)
		}
		if err := c.validateCost("units.json", u.ID, u.Cost); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateCombat() error {
	if c.Combat.DefenseModifierNoWallsPerMille <= 0 || c.Combat.DefenseModifierMaxWallsPerMille <= 0 {
		return fmt.Errorf("combat.json: defense modifiers must be positive")
	}
	if c.Combat.DefenseModifierMaxWallsPerMille < c.Combat.DefenseModifierNoWallsPerMille {
		return fmt.Errorf("combat.json: max-walls modifier must not be below no-walls modifier")
	}
	if c.Combat.RaidSuperiorityThresholdPerMille <= 1000 {
		return fmt.Errorf("combat.json: raidSuperiorityThresholdPerMille must be above 1000")
	}
	if c.Combat.RaidLossFactorPerMille <= 0 || c.Combat.RaidLossFactorPerMille > 1000 {
		return fmt.Errorf("combat.json: raidLossFactorPerMille must be in (0, 1000]")
	}
	if len(c.Combat.ProvisionTiers) == 0 {
		return fmt.Errorf("combat.json: empty provisionTiers")
	}
	prev := int64(-1)
	for i, t := range c.Combat.ProvisionTiers {
		if t.FoodPerUnit <= 0 {
			return fmt.Errorf("combat.json: provisionTiers[%d]: foodPerUnit must be positive", i)
		}
		if t.MaxDistanceCells < 0 {
			return fmt.Errorf("combat.json: provisionTiers[%d]: negative maxDistanceCells", i)
		}
		isLast := i == len(c.Combat.ProvisionTiers)-1
		if t.MaxDistanceCells == 0 && !isLast {
			return fmt.Errorf("combat.json: provisionTiers[%d]: unlimited tier (0) must be last", i)
		}
		if isLast && t.MaxDistanceCells != 0 {
			return fmt.Errorf("combat.json: provisionTiers: last tier must be unlimited (maxDistanceCells 0)")
		}
		if t.MaxDistanceCells != 0 && t.MaxDistanceCells <= prev {
			return fmt.Errorf("combat.json: provisionTiers[%d]: tiers must ascend by maxDistanceCells", i)
		}
		prev = t.MaxDistanceCells
	}
	return nil
}

func (c *Config) validateWorld() error {
	w := c.World
	if w.MapSize <= 0 {
		return fmt.Errorf("world.json: mapSize must be positive")
	}
	if w.CampCountMin <= 0 || w.CampCountMax < w.CampCountMin {
		return fmt.Errorf("world.json: invalid camp count range %d..%d", w.CampCountMin, w.CampCountMax)
	}
	if w.CampArmyMin <= 0 || w.CampArmyMax < w.CampArmyMin {
		return fmt.Errorf("world.json: invalid camp army range %d..%d", w.CampArmyMin, w.CampArmyMax)
	}
	if w.BarbarianAttackIntervalHours <= 0 {
		return fmt.Errorf("world.json: barbarianAttackIntervalHours must be positive")
	}
	if w.SnapshotIntervalSeconds <= 0 {
		return fmt.Errorf("world.json: snapshotIntervalSeconds must be positive")
	}
	if w.SnapshotPath == "" || w.EventLogPath == "" {
		return fmt.Errorf("world.json: snapshotPath and eventLogPath must be set")
	}
	if w.StorageCapacityFood <= 0 || w.StorageCapacityNonFood <= 0 {
		return fmt.Errorf("world.json: storage capacities must be positive")
	}
	if w.BaseProductionPerHour < 0 {
		return fmt.Errorf("world.json: baseProductionPerHour must not be negative")
	}
	if w.WindowHours <= 0 || w.WindowsPerPlayer <= 0 {
		return fmt.Errorf("world.json: windowHours and windowsPerPlayer must be positive")
	}
	if int64(w.WindowsPerPlayer)*w.WindowHours > 24 {
		return fmt.Errorf("world.json: battle windows exceed 24 hours")
	}
	if err := c.validateCost("world.json", "startResources", w.StartResources); err != nil {
		return err
	}
	if err := c.validateCost("world.json", "campRewardStock", w.CampRewardStock); err != nil {
		return err
	}
	for resID, chance := range w.DepositChancePerMille {
		if _, ok := c.ResourceByID[resID]; !ok {
			return fmt.Errorf("world.json: depositChancePerMille references unknown resource %q", resID)
		}
		if chance < 0 || chance > 1000 {
			return fmt.Errorf("world.json: depositChancePerMille for %q out of range [0, 1000]", resID)
		}
	}
	return nil
}
