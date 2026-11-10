package sim

import (
	"fmt"

	"inspiredtk/server/internal/config"
	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/util"
)

// NewWorld генерирует новый мир по конфигу: карта mapSize x mapSize,
// месторождения и лагеря варваров - детерминированно по seed из конфига.
// Parameters:
//   - cfg: валидированные конфиги
//   - now: серверное время создания мира
func NewWorld(cfg *config.Config, now int64) *domain.World {
	size := cfg.World.MapSize
	half := size / 2
	w := &domain.World{
		Seed:        cfg.World.Seed,
		CreatedUnix: now,
		MapSize:     size,
		Cells:       make([]*domain.Cell, 0, size*size),
		Players:     map[string]*domain.Player{},
		Villages:    map[string]*domain.Village{},
		Camps:       map[string]*domain.Camp{},
		Marches:     map[string]*domain.March{},
	}
	for y := -half; y <= half; y++ {
		for x := -half; x <= half; x++ {
			cell := &domain.Cell{X: x, Y: y}
			for _, rc := range cfg.Resources.Resources {
				chance := cfg.World.DepositChancePerMille[rc.ID]
				if chance > 0 && w.NextRandom(1000) < chance {
					cell.Deposits = append(cell.Deposits, rc.ID)
				}
			}
			w.Cells = append(w.Cells, cell)
		}
	}
	spawnCamps(cfg, w, now)
	return w
}

func spawnCamps(cfg *config.Config, w *domain.World, now int64) {
	count := cfg.World.CampCountMin +
		int(w.NextRandom(int64(cfg.World.CampCountMax-cfg.World.CampCountMin+1)))
	interval := cfg.World.BarbarianAttackIntervalHours * 3600
	for i := 0; i < count; i++ {
		cell := findFreeCell(w)
		if cell == nil {
			return
		}
		army := cfg.World.CampArmyMin +
			int(w.NextRandom(int64(cfg.World.CampArmyMax-cfg.World.CampArmyMin+1)))
		stock := make(map[string]int64, len(cfg.World.CampRewardStock))
		for resID, amount := range cfg.World.CampRewardStock {
			stock[resID] = util.UnitsToMilli(amount)
		}
		campID := fmt.Sprintf("c%d", i+1)
		w.Camps[campID] = &domain.Camp{
			ID:             campID,
			X:              cell.X,
			Y:              cell.Y,
			Infantry:       army,
			StockMilli:     stock,
			NextAttackUnix: now + interval,
		}
		cell.CampID = campID
	}
}

func findFreeCell(w *domain.World) *domain.Cell {
	half := w.MapHalf()
	for attempts := 0; attempts < 1000; attempts++ {
		x := int(w.NextRandom(int64(w.MapSize))) - half
		y := int(w.NextRandom(int64(w.MapSize))) - half
		cell := w.CellAt(x, y)
		if cell.CampID == "" && cell.VillageID == "" {
			return cell
		}
	}
	return nil
}
