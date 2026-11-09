package sim

import (
	"fmt"
	"sort"

	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/util"
)

// settle досчитывает производство деревни по прошедшему серверному времени
// (лениво, по формуле - не циклом по тикам, см. GAME/quality-attributes.md).
// Вызывается перед любым чтением/списанием ресурсов деревни, поэтому
// начисление и трата атомарны в одном расчёте - без дюпов.
// Порядок обхода зданий отсортирован: итог при переполнении склада
// детерминирован.
func (e *Engine) settle(v *domain.Village, now int64) {
	if now <= v.LastSettledUnix {
		return
	}
	dt := now - v.LastSettledUnix
	// Базовая выработка: каждая деревня производит немного каждого ресурса
	// даже без зданий (GAME/game-design.md, MVP-0).
	if base := e.Cfg.World.BaseProductionPerHour; base > 0 {
		baseMilli := util.PerHourToMilli(base)
		for _, rc := range e.Cfg.Resources.Resources {
			e.addCapped(v, rc.ID, util.AccrueMilli(baseMilli, dt))
		}
	}
	keys := sortedKeys(v.Buildings)
	for _, key := range keys {
		level := v.Buildings[key]
		if level <= 0 {
			continue
		}
		base, resource := parseBuildingKey(key)
		bc, ok := e.Cfg.BuildingByID[base]
		if !ok {
			continue
		}
		producedResource := bc.ProductionResource
		if bc.IsMine {
			producedResource = resource
		}
		if producedResource == "" {
			continue
		}
		perHour := bc.Levels[level-1].ProductionPerHour
		e.addCapped(v, producedResource, util.AccrueMilli(util.PerHourToMilli(perHour), dt))
	}
	v.LastSettledUnix = now
}

// addCapped добавляет ресурс с обрезкой по вместимости склада своей категории
// (продовольственный / непродовольственный, общий объём внутри категории).
// Переполнение в MVP-0 обрезается; гниение - MVP-1.
func (e *Engine) addCapped(v *domain.Village, resourceID string, amountMilli int64) {
	if amountMilli <= 0 {
		return
	}
	category := e.Cfg.ResourceByID[resourceID].Category
	var capMilli int64
	if category == domain.CategoryFood {
		capMilli = util.UnitsToMilli(e.Cfg.World.StorageCapacityFood)
	} else {
		capMilli = util.UnitsToMilli(e.Cfg.World.StorageCapacityNonFood)
	}
	var total int64
	for _, rc := range e.Cfg.Resources.Resources {
		if rc.Category == category {
			total += v.ResourcesMilli[rc.ID]
		}
	}
	free := capMilli - total
	if free <= 0 {
		return
	}
	if amountMilli > free {
		amountMilli = free
	}
	v.ResourcesMilli[resourceID] += amountMilli
}

// spend атомарно списывает стоимость: сначала проверка всех ресурсов,
// потом списание. При нехватке состояние не меняется.
func (e *Engine) spend(v *domain.Village, cost map[string]int64, multiplier int64) error {
	keys := make([]string, 0, len(cost))
	for resID := range cost {
		keys = append(keys, resID)
	}
	sort.Strings(keys)
	for _, resID := range keys {
		need := cost[resID] * multiplier * util.MilliPerUnit
		if v.ResourcesMilli[resID] < need {
			return fmt.Errorf("village %s: insufficient %s: need %d milli, have %d milli",
				v.ID, resID, need, v.ResourcesMilli[resID])
		}
	}
	for _, resID := range keys {
		v.ResourcesMilli[resID] -= cost[resID] * multiplier * util.MilliPerUnit
	}
	return nil
}
