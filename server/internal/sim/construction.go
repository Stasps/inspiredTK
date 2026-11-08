package sim

import (
	"errors"
	"fmt"

	"inspiredtk/server/internal/domain"
)

// StartConstruction ставит улучшение здания в очередь строительства.
// Ключ здания - id из конфига; для шахты - "mine:<resource>" по месторождению
// клетки (механика шахты зафиксирована в GAME/game-design.md).
func (e *Engine) StartConstruction(playerID, villageID, buildingKey string, now int64) error {
	v, err := e.ownedVillage(playerID, villageID)
	if err != nil {
		return err
	}
	base, resource := parseBuildingKey(buildingKey)
	bc, ok := e.Cfg.BuildingByID[base]
	if !ok {
		return fmt.Errorf("unknown building %q", base)
	}
	e.applyConstruction(v, now)
	if v.BuildJob != nil {
		return fmt.Errorf("village %s: build queue is busy", villageID)
	}
	current := v.Buildings[buildingKey]
	if current >= bc.MaxLevel {
		return fmt.Errorf("village %s: building %q already at max level %d", villageID, buildingKey, bc.MaxLevel)
	}
	if bc.IsMine {
		if resource == "" {
			return fmt.Errorf("village %s: mine requires a deposit resource, e.g. \"mine:stone\"", villageID)
		}
		if _, ok := e.Cfg.ResourceByID[resource]; !ok {
			return fmt.Errorf("village %s: unknown deposit resource %q", villageID, resource)
		}
		cell := e.World.CellAt(v.X, v.Y)
		if cell == nil || !cell.HasDeposit(resource) {
			return fmt.Errorf("village %s: no %s deposit at (%d,%d)", villageID, resource, v.X, v.Y)
		}
	}
	target := current + 1
	lvl := bc.Levels[target-1]
	e.settle(v, now)
	if err := e.spend(v, lvl.Cost, 1); err != nil {
		return err
	}
	v.BuildJob = &domain.BuildJob{
		BuildingKey: buildingKey,
		TargetLevel: target,
		FinishUnix:  now + e.effectiveBuildSeconds(v, lvl.BuildSeconds),
	}
	return nil
}

// effectiveBuildSeconds применяет ускорение строительства от уровня
// главного здания (фактор в промилле из конфига).
func (e *Engine) effectiveBuildSeconds(v *domain.Village, baseSeconds int64) int64 {
	factor := int64(1000)
	if mainLevel := v.Buildings["main"]; mainLevel > 0 {
		if mc, ok := e.Cfg.BuildingByID["main"]; ok {
			if f := mc.Levels[mainLevel-1].BuildFactorPerMille; f > 0 {
				factor = f
			}
		}
	}
	secs := baseSeconds * factor / 1000
	if secs < 1 {
		secs = 1
	}
	return secs
}

// applyConstruction завершает стройку, если её время вышло. Производство
// до момента завершения считается по старому уровню.
func (e *Engine) applyConstruction(v *domain.Village, now int64) {
	job := v.BuildJob
	if job == nil || now < job.FinishUnix {
		return
	}
	e.settle(v, job.FinishUnix)
	v.Buildings[job.BuildingKey] = job.TargetLevel
	v.BuildJob = nil
}

// slotsCapacity - лимит слотов юнитов: казарма и амбар дают слоты за уровень.
func (e *Engine) slotsCapacity(v *domain.Village) int {
	total := 0
	for key, level := range v.Buildings {
		if level <= 0 {
			continue
		}
		base, _ := parseBuildingKey(key)
		bc, ok := e.Cfg.BuildingByID[base]
		if !ok {
			continue
		}
		for lvl := 1; lvl <= level; lvl++ {
			total += bc.Levels[lvl-1].Slots
		}
	}
	return total
}

// StartTraining ставит пехотинцев в очередь тренировки казармы.
func (e *Engine) StartTraining(playerID, villageID string, count int, now int64) error {
	if count <= 0 {
		return errors.New("training count must be positive")
	}
	v, err := e.ownedVillage(playerID, villageID)
	if err != nil {
		return err
	}
	e.applyConstruction(v, now)
	e.applyTraining(v, now)
	if v.Buildings["barracks"] < 1 {
		return fmt.Errorf("village %s: barracks level 1 required for training", villageID)
	}
	if v.StationedInfantry+v.TrainQueueRemaining+count > e.slotsCapacity(v) {
		return fmt.Errorf("village %s: not enough unit slots", villageID)
	}
	uc := e.Cfg.UnitByID["infantry"]
	e.settle(v, now)
	if err := e.spend(v, uc.Cost, int64(count)); err != nil {
		return err
	}
	if v.TrainQueueRemaining == 0 {
		v.TrainNextFinishUnix = now + uc.TrainSeconds
	}
	v.TrainQueueRemaining += count
	return nil
}

// applyTraining переносит натренировавшихся юнитов в гарнизон.
func (e *Engine) applyTraining(v *domain.Village, now int64) {
	uc := e.Cfg.UnitByID["infantry"]
	for v.TrainQueueRemaining > 0 && now >= v.TrainNextFinishUnix {
		v.TrainQueueRemaining--
		v.StationedInfantry++
		v.TrainNextFinishUnix += uc.TrainSeconds
	}
	if v.TrainQueueRemaining == 0 {
		v.TrainNextFinishUnix = 0
	}
}

// InstantComplete - тестовый режим «завершить немедленно»
// (GAME/game-design.md, «Тестовый режим (MVP-0)»): мгновенно завершает
// строительство и тренировку в деревне. Доступно только при testMode.
func (e *Engine) InstantComplete(playerID, villageID string, now int64) error {
	if !e.Cfg.World.TestMode {
		return errors.New("instant complete is disabled: testMode is off")
	}
	v, err := e.ownedVillage(playerID, villageID)
	if err != nil {
		return err
	}
	if v.BuildJob != nil {
		v.BuildJob.FinishUnix = now
		e.applyConstruction(v, now)
	}
	e.settle(v, now)
	v.StationedInfantry += v.TrainQueueRemaining
	v.TrainQueueRemaining = 0
	v.TrainNextFinishUnix = 0
	return nil
}
