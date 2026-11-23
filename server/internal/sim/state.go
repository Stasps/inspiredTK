// Package sim provides game simulation logic for Inspired Kingdoms.
package sim

import (
	"fmt"

	"inspiredtk/server/internal/domain"
)

// SettlementState represents complete settlement state for client broadcast.
type SettlementState struct {
	ResourcesMilli      map[string]int64 `json:"resourcesMilli"`
	ProductionPerHour   map[string]int64 `json:"productionPerHour"`
	Buildings           []BuildingInfo   `json:"buildings"`
	StationedInfantry   int              `json:"stationedInfantry"`
	BuildJob            *domain.BuildJob `json:"buildJob,omitempty"`
	TrainQueueRemaining int              `json:"trainQueueRemaining"`
	TrainNextFinishUnix int64            `json:"trainNextFinishUnix"`
	ServerTimeUnix      int64            `json:"serverTimeUnix"`
}

// GetSettlementState returns complete settlement state for client broadcast.
// Includes resources, buildings, army, and active jobs with server time
// for client-side progress calculation.
//
// Parameters:
//   - settlementID: settlement identifier
//   - now: current server unix timestamp
//
// Returns: SettlementState pointer or error if settlement not found.
func (e *Engine) GetSettlementState(settlementID string, now int64) (*SettlementState, error) {
	v, ok := e.World.Settlements[settlementID]
	if !ok {
		return nil, fmt.Errorf("unknown settlement %q", settlementID)
	}
	e.settle(v, now)
	return &SettlementState{
		ResourcesMilli:      v.ResourcesMilli,
		ProductionPerHour:   e.productionRate(v),
		Buildings:           e.AvailableBuildings(v, now),
		StationedInfantry:   v.StationedInfantry,
		BuildJob:            v.BuildJob,
		TrainQueueRemaining: v.TrainQueueRemaining,
		TrainNextFinishUnix: v.TrainNextFinishUnix,
		ServerTimeUnix:      now,
	}, nil
}

// productionRate calculates current production per hour for each resource.
// Base production is 10/hour for all resources, plus building bonuses.
//
// Parameters:
//   - v: settlement pointer (must be settled)
//
// Returns: map of resource ID to production per hour.
func (e *Engine) productionRate(v *domain.Settlement) map[string]int64 {
	rates := make(map[string]int64)

	// Base production: 10/hour for all basic resources
	for _, res := range e.Cfg.Resources.Resources {
		rates[res.ID] = 10
	}

	// Building bonuses
	for _, bc := range e.Cfg.Buildings.Buildings {
		if bc.ProductionResource == "" {
			continue
		}
		level := v.Buildings[bc.ID]
		if level > 0 && level <= len(bc.Levels) {
			production := bc.Levels[level-1].ProductionPerHour
			rates[bc.ProductionResource] += production
		}
	}

	return rates
}

// clientState wraps world with settlement buildings for client broadcast.
type clientState struct {
	*domain.World
	Settlement *SettlementState `json:"settlement,omitempty"`
}

// GetClientState returns complete state for client broadcast.
// If settlementID is empty, returns world only without settlement details.
//
// Parameters:
//   - settlementID: settlement identifier (optional)
//   - now: current server unix timestamp
//
// Returns: clientState for broadcast.
func (e *Engine) GetClientState(settlementID string, now int64) interface{} {
	s := clientState{World: e.World}
	if settlementID != "" {
		if v, err := e.GetSettlementState(settlementID, now); err == nil {
			s.Settlement = v
		}
	}
	return s
}
