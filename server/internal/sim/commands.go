package sim

import (
	"fmt"
	"time"

	"inspiredtk/server/internal/util"
)

// MapCell - клетка карты для отображения клиенту.
type MapCell struct {
	X        int    `json:"x"`
	Y        int    `json:"y"`
	Type     string `json:"type"` // "empty", "village", "camp", "deposit"
	ID       string `json:"id,omitempty"`
	Owner    string `json:"owner,omitempty"`
	Resource string `json:"resource,omitempty"` // для deposit: wood, stone, iron
}

// GetMap возвращает карту мира для отображения.
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

// AddResources добавляет ресурсы деревне (debug-команда add_resources).
// amounts - в целых единицах (не мили). Добавление идёт без обрезки
// вместимостью склада. Доступно только при debugMode.
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

// UpgradeBuilding ставит улучшение здания в очередь строительства.
// Уровень повышается по истечении времени строительства; в тестовом режиме
// его мгновенно завершает finish_now (InstantComplete). Ключ здания - id из
// конфига; для шахты - "mine:<resource>" по месторождению клетки.
func (e *Engine) UpgradeBuilding(villageID, buildingID string) error {
	v, ok := e.World.Villages[villageID]
	if !ok {
		return fmt.Errorf("unknown village %q", villageID)
	}
	return e.startConstruction(v, buildingID, time.Now().Unix())
}

// TrainUnits тренирует юнитов в казарме. Юниты попадают в гарнизон по
// истечении времени тренировки каждого юнита.
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
