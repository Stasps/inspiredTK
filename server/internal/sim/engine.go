// Package sim - слой Game Logic: серверная симуляция мира.
// Engine не потокобезопасен: все вызовы обязаны идти из одной горутины
// (актор мира с почтовым ящиком команд, см. GAME/architecture.md,
// «Опасные места» - глобальный мьютекс запрещён).
package sim

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"inspiredtk/server/internal/config"
	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/persistence"
	"inspiredtk/server/internal/util"
)

// Engine выполняет команды и события планировщика над состоянием мира.
type Engine struct {
	Cfg    *config.Config
	World  *domain.World
	events *persistence.EventLog
}

// NewEngine собирает движок симуляции.
// Parameters:
//   - cfg: валидированные конфиги
//   - world: состояние мира (новое или восстановленное из снапшота)
//   - events: журнал критичных событий
func NewEngine(cfg *config.Config, world *domain.World, events *persistence.EventLog) *Engine {
	e := &Engine{Cfg: cfg, World: world, events: events}
	e.restoreSchedule()
	return e
}

func (e *Engine) newID(prefix string) string {
	e.World.IDCounter++
	return prefix + strconv.FormatInt(e.World.IDCounter, 10)
}

func (e *Engine) logBattle(m *domain.March, defenderID string, defBefore int, out BattleOutcome, now int64) {
	attackerID := m.PlayerID
	if m.OriginCampID != "" {
		attackerID = m.OriginCampID
	}
	err := e.events.LogBattle(persistence.BattleEvent{
		EventID:        m.EventID,
		TimeUnix:       now,
		Mode:           string(m.Type),
		MarchID:        m.ID,
		AttackerID:     attackerID,
		DefenderID:     defenderID,
		AttackerBefore: m.Infantry,
		DefenderBefore: defBefore,
		AttackerLosses: out.AttackerLosses,
		DefenderLosses: out.DefenderLosses,
		AttackerWon:    out.AttackerWon,
	})
	if err != nil {
		// Журнал критичен, но падение симуляции хуже: ошибка идёт в panic,
		// чтобы её увидели при разработке, а не потеряли молча.
		panic(fmt.Errorf("event log: %w", err))
	}
}

// parseBuildingKey разбирает ключ здания: "mine:iron" -> ("mine", "iron").
func parseBuildingKey(key string) (base, resource string) {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return key, ""
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ownedSettlement возвращает поселение, если она принадлежит игроку.
func (e *Engine) ownedSettlement(playerID, settlementID string) (*domain.Settlement, error) {
	v, ok := e.World.Settlements[settlementID]
	if !ok {
		return nil, fmt.Errorf("unknown settlement %q", settlementID)
	}
	if v.PlayerID != playerID {
		return nil, fmt.Errorf("settlement %q does not belong to player %q", settlementID, playerID)
	}
	return v, nil
}

// CreatePlayer регистрирует игрока и ставит его стартовую поселение на клетку.
// Parameters:
//   - id, name: идентификатор и имя аккаунта
//   - x, y: выбранная игроком клетка карты
//   - window1, window2: часы начала двух боевых окон (UTC)
//   - now: серверное время
func (e *Engine) CreatePlayer(id, name string, x, y, window1, window2 int, now int64) error {
	if id == "" {
		return errors.New("player id must not be empty")
	}
	if _, exists := e.World.Players[id]; exists {
		return fmt.Errorf("player %q already exists", id)
	}
	for _, h := range []int{window1, window2} {
		if h < 0 || h > 23 {
			return fmt.Errorf("player %q: battle window start hour %d out of range [0, 23]", id, h)
		}
	}
	cell := e.World.CellAt(x, y)
	if cell == nil {
		return fmt.Errorf("player %q: cell (%d,%d) is outside the map", id, x, y)
	}
	if cell.SettlementID != "" || cell.CampID != "" {
		return fmt.Errorf("player %q: cell (%d,%d) is occupied", id, x, y)
	}
	resources := make(map[string]int64, len(e.Cfg.Resources.Resources))
	for _, rc := range e.Cfg.Resources.Resources {
		resources[rc.ID] = util.UnitsToMilli(e.Cfg.World.StartResources[rc.ID])
	}
	settlementID := e.newID("v")
	e.World.Settlements[settlementID] = &domain.Settlement{
		ID:              settlementID,
		PlayerID:        id,
		X:               x,
		Y:               y,
		Buildings:       map[string]int{"main": 1},
		ResourcesMilli:  resources,
		LastSettledUnix: now,
	}
	e.World.Players[id] = &domain.Player{
		ID:         id,
		Name:       name,
		Windows:    [2]int{window1, window2},
		SettlementIDs: []string{settlementID},
	}
	cell.SettlementID = settlementID
	return nil
}

// SetWindows меняет боевые окна игрока.
func (e *Engine) SetWindows(playerID string, window1, window2 int) error {
	p, ok := e.World.Players[playerID]
	if !ok {
		return fmt.Errorf("unknown player %q", playerID)
	}
	for _, h := range []int{window1, window2} {
		if h < 0 || h > 23 {
			return fmt.Errorf("player %q: battle window start hour %d out of range [0, 23]", playerID, h)
		}
	}
	p.Windows = [2]int{window1, window2}
	return nil
}
