package sim

import (
	"errors"
	"fmt"
)

// Вход игрока без настоящей авторизации (задача 1.1 в TODO.md): соединение
// называет id игрока, а сервер в debugMode создаёт незнакомого игрока со
// стартовым поселением. Регистрация и авторизация - отдельная задача.

// Боевые окна нового игрока по умолчанию (часы начала, UTC).
const (
	defaultWindow1 = 10
	defaultWindow2 = 19
)

// SettlementRef - поселение игрока в приветствии клиенту.
type SettlementRef struct {
	ID string `json:"id"`
	X  int    `json:"x"`
	Y  int    `json:"y"`
}

// PlayerInfo - приветствие сессии: кто играет и какие поселения его.
type PlayerInfo struct {
	PlayerID       string          `json:"playerId"`
	Name           string          `json:"name"`
	Settlements    []SettlementRef `json:"settlements"`
	ServerTimeUnix int64           `json:"serverTimeUnix"`
}

// JoinPlayer сопоставляет подключение с игроком. Известный игрок входит
// всегда; незнакомого в debugMode создаёт со стартовым поселением на
// ближайшей к центру свободной клетке, без debugMode отказывает.
// Parameters:
//   - playerID: id игрока из подключения
//   - now: серверное время
//
// Returns: приветствие для клиента или ошибка.
func (e *Engine) JoinPlayer(playerID string, now int64) (*PlayerInfo, error) {
	if playerID == "" {
		return nil, errors.New("player id must not be empty")
	}
	if _, ok := e.World.Players[playerID]; !ok {
		if !e.Cfg.World.DebugMode {
			return nil, fmt.Errorf("unknown player %q: registration is not implemented", playerID)
		}
		if err := e.SpawnPlayer(playerID, playerID, now); err != nil {
			return nil, err
		}
	}
	return e.PlayerInfo(playerID, now)
}

// SpawnPlayer создаёт игрока на ближайшей к центру карты (0,0) свободной
// клетке. Клетка с месторождением предпочтительнее, чтобы шахта была
// доступна сразу; если таких нет - любая свободная.
func (e *Engine) SpawnPlayer(id, name string, now int64) error {
	bestDeposit, bestFree := -1, -1
	var bestDepositDist, bestFreeDist int64
	for i, c := range e.World.Cells {
		if c.SettlementID != "" || c.CampID != "" {
			continue
		}
		d := int64(c.X)*int64(c.X) + int64(c.Y)*int64(c.Y)
		if len(c.Deposits) > 0 && (bestDeposit < 0 || d < bestDepositDist) {
			bestDeposit, bestDepositDist = i, d
		}
		if bestFree < 0 || d < bestFreeDist {
			bestFree, bestFreeDist = i, d
		}
	}
	idx := bestDeposit
	if idx < 0 {
		idx = bestFree
	}
	if idx < 0 {
		return fmt.Errorf("player %q: no free cell on the map", id)
	}
	c := e.World.Cells[idx]
	return e.CreatePlayer(id, name, c.X, c.Y, defaultWindow1, defaultWindow2, now)
}

// PlayerInfo возвращает приветствие для игрока.
func (e *Engine) PlayerInfo(playerID string, now int64) (*PlayerInfo, error) {
	p, ok := e.World.Players[playerID]
	if !ok {
		return nil, fmt.Errorf("unknown player %q", playerID)
	}
	refs := make([]SettlementRef, 0, len(p.SettlementIDs))
	for _, id := range p.SettlementIDs {
		if v, ok := e.World.Settlements[id]; ok {
			refs = append(refs, SettlementRef{ID: v.ID, X: v.X, Y: v.Y})
		}
	}
	return &PlayerInfo{PlayerID: p.ID, Name: p.Name, Settlements: refs, ServerTimeUnix: now}, nil
}

// PlayerState возвращает состояние для рассылки игроку: мир и подробное
// состояние его первого поселения. Других поселений пока не бывает;
// выбор поселения в клиенте - вместе с основанием новых.
func (e *Engine) PlayerState(playerID string, now int64) interface{} {
	settlementID := ""
	if p, ok := e.World.Players[playerID]; ok && len(p.SettlementIDs) > 0 {
		settlementID = p.SettlementIDs[0]
	}
	return e.GetClientState(settlementID, now)
}
