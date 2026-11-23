package sim

import (
	"errors"
	"fmt"

	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/util"
)

// provisionPerUnit - тариф провианта (еда на юнита) по дистанции марша.
func (e *Engine) provisionPerUnit(distMilli int64) int64 {
	for _, t := range e.Cfg.Combat.ProvisionTiers {
		if t.MaxDistanceCells == 0 || distMilli <= t.MaxDistanceCells*util.MilliPerUnit {
			return t.FoodPerUnit
		}
	}
	return 0
}

// SendMarch отправляет марш (атаку или набег) из поселения на клетку с лагерем
// варваров или чужой поселением. Провиант списывается при отправке
// (GAME/game-design.md, «Марши и провиант»).
//
// Returns: id созданного марша.
func (e *Engine) SendMarch(playerID, settlementID string, marchType domain.MarchType, toX, toY, units int, now int64) (string, error) {
	if marchType != domain.MarchAttack && marchType != domain.MarchRaid {
		return "", fmt.Errorf("march type must be %q or %q", domain.MarchAttack, domain.MarchRaid)
	}
	if units <= 0 {
		return "", errors.New("march units must be positive")
	}
	v, err := e.ownedSettlement(playerID, settlementID)
	if err != nil {
		return "", err
	}
	e.applyConstruction(v, now)
	e.applyTraining(v, now)
	if v.StationedInfantry < units {
		return "", fmt.Errorf("settlement %s: not enough infantry: need %d, have %d", settlementID, units, v.StationedInfantry)
	}
	cell := e.World.CellAt(toX, toY)
	if cell == nil {
		return "", fmt.Errorf("target cell (%d,%d) is outside the map", toX, toY)
	}
	var targetSettlementID, targetCampID string
	switch {
	case cell.CampID != "":
		targetCampID = cell.CampID
	case cell.SettlementID != "":
		if cell.SettlementID == settlementID {
			return "", errors.New("cannot march on own settlement")
		}
		targetSettlementID = cell.SettlementID
	default:
		return "", fmt.Errorf("no target at cell (%d,%d)", toX, toY)
	}
	distMilli := util.DistanceMilli(v.X, v.Y, toX, toY)
	provision := e.provisionPerUnit(distMilli) * int64(units) * util.MilliPerUnit
	e.settle(v, now)
	if v.ResourcesMilli["food"] < provision {
		return "", fmt.Errorf("settlement %s: not enough food for provisions: need %d milli, have %d milli",
			settlementID, provision, v.ResourcesMilli["food"])
	}
	v.ResourcesMilli["food"] -= provision
	v.StationedInfantry -= units

	id := e.newID("m")
	e.addMarch(&domain.March{
		ID:              id,
		PlayerID:        playerID,
		Type:            marchType,
		Infantry:        units,
		FromX:           v.X,
		FromY:           v.Y,
		ToX:             toX,
		ToY:             toY,
		OriginSettlementID: settlementID,
		TargetSettlementID: targetSettlementID,
		TargetCampID:    targetCampID,
		DepartUnix:      now,
		ArriveUnix:      now + util.TravelSeconds(distMilli, e.Cfg.UnitByID["infantry"].SpeedCellsPerHour),
		ProvisionMilli:  provision,
	})
	return id, nil
}

// addMarch регистрирует марш в мире и ставит событие его прибытия.
func (e *Engine) addMarch(m *domain.March) {
	m.EventID = e.schedule(domain.EventMarchArrive, m.ID, m.ArriveUnix)
	e.World.Marches[m.ID] = m
}

// CancelMarch отменяет марш до прибытия: провиант возвращается в поселение,
// отряд разворачивается и идёт домой (GAME/game-design.md, «Марши и провиант»).
func (e *Engine) CancelMarch(playerID, marchID string, now int64) error {
	m, ok := e.World.Marches[marchID]
	if !ok {
		return fmt.Errorf("unknown march %q", marchID)
	}
	if m.PlayerID != playerID {
		return fmt.Errorf("march %q does not belong to player %q", marchID, playerID)
	}
	if m.Type == domain.MarchReturn {
		return fmt.Errorf("march %q is a return march and cannot be cancelled", marchID)
	}
	if now >= m.ArriveUnix {
		return fmt.Errorf("march %q has already arrived", marchID)
	}
	origin := e.World.Settlements[m.OriginSettlementID]
	if origin == nil {
		return fmt.Errorf("march %q: origin settlement %q is gone", marchID, m.OriginSettlementID)
	}
	e.settle(origin, now)
	e.addCapped(origin, "food", m.ProvisionMilli)
	delete(e.World.Marches, marchID)

	elapsed := now - m.DepartUnix
	returnID := e.newID("m")
	e.addMarch(&domain.March{
		ID:              returnID,
		PlayerID:        playerID,
		Type:            domain.MarchReturn,
		Infantry:        m.Infantry,
		FromX:           m.ToX,
		FromY:           m.ToY,
		ToX:             origin.X,
		ToY:             origin.Y,
		OriginSettlementID: origin.ID,
		DepartUnix:      now,
		ArriveUnix:      now + elapsed,
	})
	return nil
}

// resolveMarch обрабатывает прибытие марша. Вызывается планировщиком по
// событию march_arrive; now - время прибытия (FireUnix события).
func (e *Engine) resolveMarch(m *domain.March, now int64) {
	if m.Type == domain.MarchReturn {
		e.resolveReturn(m, now)
		return
	}
	if m.TargetCampID != "" {
		e.resolveCampBattle(m, now)
		return
	}
	e.resolveSettlementBattle(m, now)
}

// resolveReturn возвращает отряд домой: юниты в гарнизон, добыча на склад.
func (e *Engine) resolveReturn(m *domain.March, now int64) {
	v := e.World.Settlements[m.OriginSettlementID]
	if v == nil {
		return
	}
	e.settle(v, now)
	v.StationedInfantry += m.Infantry
	for _, resID := range sortedKeys(m.LootMilli) {
		e.addCapped(v, resID, m.LootMilli[resID])
	}
}

// resolveCampBattle - прибытие марша игрока к лагерю варваров. При победе
// выжившие забирают запас лагеря и идут домой.
func (e *Engine) resolveCampBattle(m *domain.March, now int64) {
	camp := e.World.Camps[m.TargetCampID]
	if camp == nil {
		e.sendSurvivorsHome(m, m.Infantry, nil, now)
		return
	}
	out := ResolveBattle(m.Infantry, camp.Infantry, m.Type, e.battleParams())
	e.logBattle(m, camp.ID, camp.Infantry, out, now)
	camp.Infantry -= out.DefenderLosses
	survivors := m.Infantry - out.AttackerLosses
	var loot map[string]int64
	if out.AttackerWon && len(camp.StockMilli) > 0 {
		loot = camp.StockMilli
		camp.StockMilli = map[string]int64{}
	}
	e.sendSurvivorsHome(m, survivors, loot, now)
}

// resolveSettlementBattle - прибытие марша к поселению (варварская атака или PvP).
// Выжившие варвары возвращаются в лагерь, выжившие игроки - домой.
func (e *Engine) resolveSettlementBattle(m *domain.March, now int64) {
	v := e.World.Settlements[m.TargetSettlementID]
	if v == nil {
		e.sendSurvivorsHome(m, m.Infantry, nil, now)
		return
	}
	e.applyTraining(v, now)
	out := ResolveBattle(m.Infantry, v.StationedInfantry, m.Type, e.battleParams())
	e.logBattle(m, v.ID, v.StationedInfantry, out, now)
	v.StationedInfantry -= out.DefenderLosses
	survivors := m.Infantry - out.AttackerLosses
	if m.OriginCampID != "" {
		if camp := e.World.Camps[m.OriginCampID]; camp != nil {
			camp.Infantry += survivors
			// Лагерь без войск простаивает без события: вернувшиеся
			// выжившие снова ставят его в планировщик.
			if camp.EventID == "" {
				e.armCamp(camp, now)
			}
		}
		return
	}
	e.sendSurvivorsHome(m, survivors, nil, now)
}

// sendSurvivorsHome создаёт обратный марш для выживших, если они есть.
func (e *Engine) sendSurvivorsHome(m *domain.March, survivors int, loot map[string]int64, now int64) {
	if survivors <= 0 {
		return
	}
	origin := e.World.Settlements[m.OriginSettlementID]
	if origin == nil {
		return
	}
	travel := m.ArriveUnix - m.DepartUnix
	returnID := e.newID("m")
	e.addMarch(&domain.March{
		ID:              returnID,
		PlayerID:        m.PlayerID,
		Type:            domain.MarchReturn,
		Infantry:        survivors,
		FromX:           m.ToX,
		FromY:           m.ToY,
		ToX:             origin.X,
		ToY:             origin.Y,
		OriginSettlementID: origin.ID,
		DepartUnix:      now,
		ArriveUnix:      now + travel,
		LootMilli:       loot,
	})
}
