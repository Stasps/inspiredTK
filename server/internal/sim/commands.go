// Package sim provides game simulation logic for Inspired Kingdoms.
// Engine is not thread-safe: all calls must come from a single goroutine
// (world actor with command mailbox, see GAME/architecture.md).
package sim

import (
	"errors"
	"fmt"
	"time"

	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/util"
)

// SkipTime advances server time by shifting all world timers backward.
// Used for debug time skip: construction, training, marches, and barbarian
// scheduler timers are shifted. Resource accrual is lazy (settle from
// LastSettledUnix), so shifting LastSettledUnix also accrues resources
// for the skipped period.
//
// Parameters:
//   - seconds: number of seconds to skip forward
//
// Returns: error if debugMode is disabled or seconds is not positive.
func (e *Engine) SkipTime(seconds int64) error {
	if !e.Cfg.World.DebugMode {
		return errors.New("skip_time is disabled: debugMode is off")
	}
	if seconds <= 0 {
		return fmt.Errorf("skip_time: seconds must be positive, got %d", seconds)
	}
	shift := func(t int64) int64 {
		t -= seconds
		if t < 0 {
			t = 0
		}
		return t
	}
	for _, v := range e.World.Villages {
		v.LastSettledUnix = shift(v.LastSettledUnix)
		if v.BuildJob != nil {
			v.BuildJob.FinishUnix = shift(v.BuildJob.FinishUnix)
		}
		if v.TrainQueueRemaining > 0 {
			v.TrainNextFinishUnix = shift(v.TrainNextFinishUnix)
		}
	}
	for _, m := range e.World.Marches {
		m.DepartUnix = shift(m.DepartUnix)
		m.ArriveUnix = shift(m.ArriveUnix)
	}
	for _, c := range e.World.Camps {
		c.NextAttackUnix = shift(c.NextAttackUnix)
		c.ScheduledDepartUnix = shift(c.ScheduledDepartUnix)
	}
	return nil
}

// BuildingInfo represents building state for client display.
type BuildingInfo struct {
	ID         string           `json:"id"`
	Level      int              `json:"level"`
	MaxLevel   int              `json:"max_level"`
	CanUpgrade bool             `json:"can_upgrade"`
	Cost       map[string]int64 `json:"cost,omitempty"`
}

// AvailableBuildings returns all buildings from config with their village state.
// Mines (isMine) are expanded into one element per cell deposit: "mine:stone", etc.
//
// Parameters:
//   - v: village pointer (must be settled)
//   - now: current server unix timestamp
//
// Returns: slice of BuildingInfo for all buildings.
func (e *Engine) AvailableBuildings(v *domain.Village, now int64) []BuildingInfo {
	e.applyConstruction(v, now)
	e.applyTraining(v, now)
	e.settle(v, now)

	cell := e.World.CellAt(v.X, v.Y)
	infos := make([]BuildingInfo, 0, len(e.Cfg.Buildings.Buildings))
	for _, bc := range e.Cfg.Buildings.Buildings {
		keys := []string{bc.ID}
		if bc.IsMine {
			keys = keys[:0]
			if cell != nil {
				for _, dep := range cell.Deposits {
					keys = append(keys, bc.ID+":"+dep)
				}
			}
		}
		for _, key := range keys {
			level := v.Buildings[key]
			info := BuildingInfo{
				ID:       key,
				Level:    level,
				MaxLevel: bc.MaxLevel,
			}
			if level < bc.MaxLevel {
				info.Cost = bc.Levels[level].Cost
				info.CanUpgrade = v.BuildJob == nil && e.canAfford(v, info.Cost)
			}
			infos = append(infos, info)
		}
	}
	return infos
}

// canAfford checks if village has enough resources for cost.
// Village must be settled before this call.
//
// Parameters:
//   - v: village pointer
//   - cost: resource costs in whole units
//
// Returns: true if village can afford the cost.
func (e *Engine) canAfford(v *domain.Village, cost map[string]int64) bool {
	for resID, amount := range cost {
		if _, ok := e.Cfg.ResourceByID[resID]; !ok {
			return false
		}
		if v.ResourcesMilli[resID] < amount*util.MilliPerUnit {
			return false
		}
	}
	return true
}

// MapCell represents a map cell for client display.
type MapCell struct {
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Type     string `json:"type"` // "empty", "village", "camp", "deposit"
	ID       string `json:"id,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Resource string `json:"resource,omitempty"` // for deposit: wood, stone, iron
}

// GetMap returns world map for client display.
//
// Returns: slice of MapCell for all cells.
func (e *Engine) GetMap() []MapCell {
	cells := make([]MapCell, 0, len(e.World.Cells))
	for _, c := range e.World.Cells {
		mc := MapCell{X: c.X, Y: c.Y, Type: "empty"}
		switch {
		case c.VillageID != "":
			mc.Type = "village"
			mc.ID = c.VillageID
			if v, ok := e.World.Villages[c.VillageID]; ok {
				mc.Owner = v.PlayerID
			}
		case c.CampID != "":
			mc.Type = "camp"
			mc.ID = c.CampID
		case len(c.Deposits) > 0:
			mc.Type = "deposit"
			mc.Resource = c.Deposits[0]
		}
		cells = append(cells, mc)
	}
	return cells
}

// AddResources adds resources to village (debug command add_resources).
// Amounts are in whole units (not milli). No storage capacity check.
// Only available when debugMode is enabled.
//
// Parameters:
//   - villageID: village identifier
//   - amounts: map of resource ID to amount in whole units
//
// Returns: error if debugMode is off, village not found, or unknown resource.
func (e *Engine) AddResources(villageID string, amounts map[string]int64) error {
	if !e.Cfg.World.DebugMode {
		return fmt.Errorf("add_resources is disabled: debugMode is off")
	}
	v, ok := e.World.Villages[villageID]
	if !ok {
		return fmt.Errorf("unknown village %q", villageID)
	}
	e.settle(v, time.Now().Unix())
	for resID, units := range amounts {
		if _, ok := e.Cfg.ResourceByID[resID]; !ok {
			return fmt.Errorf("unknown resource %q", resID)
		}
		v.ResourcesMilli[resID] += units * util.MilliPerUnit
	}
	return nil
}

// UpgradeBuilding queues building upgrade.
// Level increases after construction time; finish_now (InstantComplete)
// completes it immediately in test mode. Building key is config ID;
// for mines - "mine:<resource>" by cell deposit.
//
// Parameters:
//   - villageID: village identifier
//   - buildingID: building config ID or "mine:<resource>" for mines
//
// Returns: error if village not found or construction cannot start.
func (e *Engine) UpgradeBuilding(villageID, buildingID string) error {
	v, ok := e.World.Villages[villageID]
	if !ok {
		return fmt.Errorf("unknown village %q", villageID)
	}
	return e.startConstruction(v, buildingID, time.Now().Unix())
}

// TrainUnits trains units in barracks.
// Units appear in garrison after each unit's training time.
//
// Parameters:
//   - villageID: village identifier
//   - unitType: unit config ID
//   - count: number of units to train
//
// Returns: error if village not found, unknown unit, or training cannot start.
func (e *Engine) TrainUnits(villageID, unitType string, count int) error {
	v, ok := e.World.Villages[villageID]
	if !ok {
		return fmt.Errorf("unknown village %q", villageID)
	}
	if _, ok := e.Cfg.UnitByID[unitType]; !ok {
		return fmt.Errorf("unknown unit %q", unitType)
	}
	return e.startTraining(v, count, time.Now().Unix())
}
