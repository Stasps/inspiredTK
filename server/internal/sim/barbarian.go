package sim

import (
	"math"
	"time"

	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/util"
)

// actCamp - PvE-логика лагеря по событию camp_act: лагерь атакует случайное
// поселение раз в barbarianAttackIntervalHours, отправка марша - только
// внутри боевых окон игрока-владельца цели (GAME/game-design.md, «PvE»).
// После действия лагерь сам ставит своё следующее событие.
func (e *Engine) actCamp(camp *domain.Camp, now int64) {
	if camp.TargetSettlementID == "" && now >= camp.NextAttackUnix && camp.Infantry > 0 {
		e.scheduleBarbarianAttack(camp, now)
	}
	if camp.TargetSettlementID != "" && now >= camp.ScheduledDepartUnix {
		e.launchBarbarianAttack(camp, now)
	}
	e.armCamp(camp, now)
}

// armCamp ставит следующее событие лагеря: отправку уже назначенной атаки
// либо планирование новой. Лагерь без войск и без цели события не получает -
// его снова поставят в планировщик вернувшиеся выжившие. Событие не ставится
// раньше now, чтобы игровое время обработчиков не шло назад.
func (e *Engine) armCamp(camp *domain.Camp, now int64) {
	var at int64
	switch {
	case camp.TargetSettlementID != "":
		at = camp.ScheduledDepartUnix
	case camp.Infantry > 0:
		at = camp.NextAttackUnix
	default:
		camp.EventID = ""
		return
	}
	if at < now {
		at = now
	}
	camp.EventID = e.schedule(domain.EventCampAct, camp.ID, at)
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
	camp.TargetSettlementID = target.ID
	camp.ScheduledDepartUnix = nextWindowStart(player.Windows, e.Cfg.World.WindowHours, now)
}

func (e *Engine) launchBarbarianAttack(camp *domain.Camp, now int64) {
	v := e.World.Settlements[camp.TargetSettlementID]
	camp.TargetSettlementID = ""
	camp.NextAttackUnix = now + e.Cfg.World.BarbarianAttackIntervalHours*3600
	if v == nil {
		return
	}
	id := e.newID("m")
	e.addMarch(&domain.March{
		ID:              id,
		Type:            domain.MarchAttack,
		Infantry:        camp.Infantry,
		FromX:           camp.X,
		FromY:           camp.Y,
		ToX:             v.X,
		ToY:             v.Y,
		OriginCampID:    camp.ID,
		TargetSettlementID: v.ID,
		DepartUnix:      now,
		ArriveUnix:      now + e.travelSeconds(camp.X, camp.Y, v.X, v.Y),
	})
	camp.Infantry = 0
}

func (e *Engine) travelSeconds(fromX, fromY, toX, toY int) int64 {
	distMilli := util.DistanceMilli(fromX, fromY, toX, toY)
	return util.TravelSeconds(distMilli, e.Cfg.UnitByID["infantry"].SpeedCellsPerHour)
}

// pickBarbarianTarget выбирает случайную поселение детерминированным RNG мира.
func (e *Engine) pickBarbarianTarget() *domain.Settlement {
	if len(e.World.Settlements) == 0 {
		return nil
	}
	ids := sortedKeys(e.World.Settlements)
	return e.World.Settlements[ids[e.World.NextRandom(int64(len(ids)))]]
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
