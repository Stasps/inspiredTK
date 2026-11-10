// Package sim provides game simulation logic for Inspired Kingdoms.
package sim

import (
	"fmt"
	"time"

	"inspiredtk/server/internal/domain"
)

// VillageState represents complete village state for client broadcast.
type VillageState struct {
	ResourcesMilli      map[string]int64 `json:"resourcesMilli"`
	Buildings           []BuildingInfo   `json:"buildings"`
	StationedInfantry   int              `json:"stationedInfantry"`
	BuildJob            *domain.BuildJob `json:"buildJob,omitempty"`
	TrainQueueRemaining int              `json:"trainQueueRemaining"`
	TrainNextFinishUnix int64            `json:"trainNextFinishUnix"`
	ServerTimeUnix      int64            `json:"serverTimeUnix"`
}

// GetVillageState returns complete village state for client broadcast.
// Includes resources, buildings, army, and active jobs with server time
// for client-side progress calculation.
//
// Parameters:
//   - villageID: village identifier
//   - now: current server unix timestamp
//
// Returns: VillageState pointer or error if village not found.
func (e *Engine) GetVillageState(villageID string, now int64) (*VillageState, error) {
	v, ok := e.World.Villages[villageID]
	if !ok {
		return nil, fmt.Errorf("unknown village %q", villageID)
	}
	e.settle(v, now)
	return &VillageState{
		ResourcesMilli:      v.ResourcesMilli,
		Buildings:           e.AvailableBuildings(v, now),
		StationedInfantry:   v.StationedInfantry,
		BuildJob:            v.BuildJob,
		TrainQueueRemaining: v.TrainQueueRemaining,
		TrainNextFinishUnix: v.TrainNextFinishUnix,
		ServerTimeUnix:      now,
	}, nil
}

// clientState wraps world with village buildings for client broadcast.
type clientState struct {
	*domain.World
	Village *VillageState `json:"village,omitempty"`
}

// GetClientState returns complete state for client broadcast.
// If villageID is empty, returns world only without village details.
//
// Parameters:
//   - villageID: village identifier (optional)
//   - now: current server unix timestamp
//
// Returns: clientState for broadcast.
func (e *Engine) GetClientState(villageID string, now int64) interface{} {
	s := clientState{World: e.World}
	if villageID != "" {
		if v, err := e.GetVillageState(villageID, now); err == nil {
			s.Village = v
		}
	}
	return s
}
