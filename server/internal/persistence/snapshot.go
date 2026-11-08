// Package persistence - слой Persistence: JSON-снапшоты состояния и
// журнал критичных событий (GAME/save-structure.md, GAME/architecture.md).
package persistence

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"inspiredtk/server/internal/domain"
)

// Snapshot - снимок состояния сервера. Поле version обязательно с первой
// версии; все количественные значения - целые числа.
type Snapshot struct {
	Version        int           `json:"version"`
	Tick           int64         `json:"tick"`
	ServerTimeUnix int64         `json:"serverTimeUnix"`
	World          *domain.World `json:"world"`
}

// NewSnapshot собирает снапшот из состояния мира.
// Parameters:
//   - w: состояние мира
//   - serverTimeUnix: серверный таймстамп снятия
//
// Returns: готовый снапшот с текущей версией формата.
func NewSnapshot(w *domain.World, serverTimeUnix int64) *Snapshot {
	return &Snapshot{
		Version:        domain.SnapshotVersion,
		Tick:           w.Tick,
		ServerTimeUnix: serverTimeUnix,
		World:          w,
	}
}

// SaveSnapshot атомарно пишет снапшот: временный файл + rename, чтобы крах
// во время записи не оставил битый снапшот.
func SaveSnapshot(path string, s *Snapshot) error {
	if s == nil || s.World == nil {
		return fmt.Errorf("save snapshot %s: nil snapshot", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("save snapshot %s: %w", path, err)
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return fmt.Errorf("save snapshot %s: %w", path, err)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("save snapshot %s: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("save snapshot %s: %w", path, err)
	}
	return nil
}

// LoadSnapshot читает снапшот и проверяет версию формата.
// Если файла нет, возвращает (nil, nil) - мир нужно генерировать заново.
func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load snapshot %s: %w", path, err)
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("load snapshot %s: %w", path, err)
	}
	if s.Version != domain.SnapshotVersion {
		return nil, fmt.Errorf("load snapshot %s: unsupported version %d (current %d)", path, s.Version, domain.SnapshotVersion)
	}
	if s.World == nil {
		return nil, fmt.Errorf("load snapshot %s: missing world", path)
	}
	return &s, nil
}

// BattleEvent - запись о бое в журнале критичных событий.
type BattleEvent struct {
	Tick           int64  `json:"tick"`
	TimeUnix       int64  `json:"timeUnix"`
	Type           string `json:"type"` // всегда "battle"
	Mode           string `json:"mode"`
	MarchID        string `json:"marchId"`
	AttackerID     string `json:"attackerId"`
	DefenderID     string `json:"defenderId"`
	AttackerBefore int    `json:"attackerBefore"`
	DefenderBefore int    `json:"defenderBefore"`
	AttackerLosses int    `json:"attackerLosses"`
	DefenderLosses int    `json:"defenderLosses"`
	AttackerWon    bool   `json:"attackerWon"`
}

// EventLog - append-only журнал критичных событий (JSONL).
type EventLog struct {
	w      io.Writer
	closer io.Closer
}

// NewEventLog оборачивает произвольный writer (для тестов).
func NewEventLog(w io.Writer) *EventLog {
	return &EventLog{w: w}
}

// OpenEventLog открывает файл журнала на дозапись.
func OpenEventLog(path string) (*EventLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("open event log %s: %w", path, err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open event log %s: %w", path, err)
	}
	return &EventLog{w: f, closer: f}, nil
}

// LogBattle добавляет запись о бое одной JSON-строкой.
func (l *EventLog) LogBattle(ev BattleEvent) error {
	ev.Type = "battle"
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("log battle: %w", err)
	}
	if _, err := l.w.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("log battle: %w", err)
	}
	return nil
}

// Close закрывает файл журнала, если он был открыт через OpenEventLog.
func (l *EventLog) Close() error {
	if l.closer == nil {
		return nil
	}
	return l.closer.Close()
}
