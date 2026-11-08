package sim

import (
	"math"
	"time"

	"inspiredtk/server/internal/domain"
)

// tickBarbarians - планировщик PvE: каждый лагерь атакует случайную деревню
// раз в barbarianAttackIntervalHours, отправка марша - только внутри боевых
// окон игрока-владельца цели (GAME/game-design.md, «PvE»).
func (e *Engine) tickBarbarians(now int64) {
	for _, id := range sortedKeys(e.World.Camps) {
		camp := e.World.Camps[id]
		if camp.TargetVillageID == "" && now >= camp.NextAttackUnix && camp.Infantry > 0 {
			e.scheduleBarbarianAttack(camp, now)
		}
		if camp.TargetVillageID != "" && now >= camp.ScheduledDepartUnix {
			e.launchBarbarianAttack(camp, now)
		}
	}
}

func (e *Engine) scheduleBarbarianAttack(camp *domain.Camp, now int64) {
	target := e.pickBarbarianTarget()
	if target == nil {
		// Деревень нет: повторить попытку через час.
		camp.NextAttackUnix = now + 3600
		return
	}
	player := e.World.Players[target.PlayerID]
	if player == nil {
		camp.NextAttackUnix = now + 3600
		return
	}
	camp.TargetVillageID = target.ID
	camp.ScheduledDepartUnix = nextWindowStart(player.Windows, e.Cfg.World.WindowHours, now)
}

func (e *Engine) launchBarbarianAttack(camp *domain.Camp, now int64) {
	v := e.World.Villages[camp.TargetVillageID]
	camp.TargetVillageID = ""
	camp.NextAttackUnix = now + e.Cfg.World.BarbarianAttackIntervalHours*3600
	if v == nil {
		return
	}
	id := e.newID("m")
	e.World.Marches[id] = &domain.March{
		ID:              id,
		Type:            domain.MarchAttack,
		Infantry:        camp.Infantry,
		FromX:           camp.X,
		FromY:           camp.Y,
		ToX:             v.X,
		ToY:             v.Y,
		OriginCampID:    camp.ID,
		TargetVillageID: v.ID,
		DepartUnix:      now,
		ArriveUnix:      now + e.travelSeconds(camp.X, camp.Y, v.X, v.Y),
	}
	camp.Infantry = 0
}

func (e *Engine) travelSeconds(fromX, fromY, toX, toY int) int64 {
	distMilli := distanceCells(fromX, fromY, toX, toY)
	return travelTime(distMilli, e.Cfg.UnitByID["infantry"].SpeedCellsPerHour)
}

// pickBarbarianTarget выбирает случайную деревню детерминированным RNG мира.
func (e *Engine) pickBarbarianTarget() *domain.Village {
	if len(e.World.Villages) == 0 {
		return nil
	}
	ids := sortedKeys(e.World.Villages)
	return e.World.Villages[ids[e.World.NextRandom(int64(len(ids)))]]
}

// nextWindowStart возвращает ближайший момент >= now внутри одного из боевых
// окон игрока. Если now уже внутри окна - возвращает now. Часы окон - UTC.
func nextWindowStart(windows [2]int, windowHours, now int64) int64 {
	t := time.Unix(now, 0).UTC()
	midnight := now - int64(t.Hour())*3600 - int64(t.Minute())*60 - int64(t.Second())
	best := int64(math.MaxInt64)
	for day := int64(0); day <= 1; day++ {
		for _, h := range windows {
			start := midnight + day*86400 + int64(h)*3600
			end := start + windowHours*3600
			if now >= start && now < end {
				return now
			}
			if start >= now && start < best {
				best = start
			}
		}
	}
	return best
}
