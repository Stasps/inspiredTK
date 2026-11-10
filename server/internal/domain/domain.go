// Package domain содержит типы состояния мира Inspired Kingdoms.
// Сериализация - только целые числа (см. GAME/save-structure.md): количества
// ресурсов хранятся в милли-единицах (x1000), время - в unix-секундах.
package domain

// SnapshotVersion - текущая версия формата снапшота (GAME/save-structure.md).
const SnapshotVersion = 1

// Категории складов (GAME/game-design.md, «Склады»).
const (
	CategoryFood    = "food"
	CategoryNonFood = "nonfood"
)

// Cell - клетка карты мира.
type Cell struct {
	X         int      `json:"x"`
	Y         int      `json:"y"`
	Deposits  []string `json:"deposits"`
	VillageID string   `json:"villageId,omitempty"`
	CampID    string   `json:"campId,omitempty"`
}

// HasDeposit сообщает, есть ли на клетке месторождение ресурса.
func (c *Cell) HasDeposit(resourceID string) bool {
	for _, d := range c.Deposits {
		if d == resourceID {
			return true
		}
	}
	return false
}

// Player - аккаунт игрока. Windows - часы начала боевых окон (UTC), каждое
// окно длиной windowHours из конфига мира.
type Player struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Windows    [2]int   `json:"windows"`
	VillageIDs []string `json:"villageIds"`
}

// BuildJob - активное строительство (один слот на деревню).
type BuildJob struct {
	BuildingKey string `json:"buildingKey"`
	TargetLevel int    `json:"targetLevel"`
	StartUnix    int64  `json:"startUnix"`
	FinishUnix   int64  `json:"finishUnix"`
}

// Village - деревня игрока. ResourcesMilli - остатки ресурсов в милли-единицах.
type Village struct {
	ID                  string           `json:"id"`
	PlayerID            string           `json:"playerId"`
	X                   int              `json:"x"`
	Y                   int              `json:"y"`
	Buildings           map[string]int   `json:"buildings"`
	ResourcesMilli      map[string]int64 `json:"resourcesMilli"`
	LastSettledUnix     int64            `json:"lastSettledUnix"`
	BuildJob            *BuildJob        `json:"buildJob,omitempty"`
	TrainQueueRemaining int              `json:"trainQueueRemaining"`
	TrainNextFinishUnix int64            `json:"trainNextFinishUnix"`
	StationedInfantry   int              `json:"stationedInfantry"`
}

// MarchType - тип марша: атака, набег или возврат домой.
type MarchType string

// Типы маршей.
const (
	MarchAttack MarchType = "attack"
	MarchRaid   MarchType = "raid"
	MarchReturn MarchType = "return"
)

// March - движущийся отряд пехотинцев.
type March struct {
	ID              string           `json:"id"`
	PlayerID        string           `json:"playerId,omitempty"`
	Type            MarchType        `json:"type"`
	Infantry        int              `json:"infantry"`
	FromX           int              `json:"fromX"`
	FromY           int              `json:"fromY"`
	ToX             int              `json:"toX"`
	ToY             int              `json:"toY"`
	OriginVillageID string           `json:"originVillageId,omitempty"`
	OriginCampID    string           `json:"originCampId,omitempty"`
	TargetVillageID string           `json:"targetVillageId,omitempty"`
	TargetCampID    string           `json:"targetCampId,omitempty"`
	DepartUnix      int64            `json:"departUnix"`
	ArriveUnix      int64            `json:"arriveUnix"`
	ProvisionMilli  int64            `json:"provisionMilli"`
	LootMilli       map[string]int64 `json:"lootMilli,omitempty"`
}

// Camp - статичный лагерь варваров (PvE, MVP-0).
type Camp struct {
	ID                  string           `json:"id"`
	X                   int              `json:"x"`
	Y                   int              `json:"y"`
	Infantry            int              `json:"infantry"`
	StockMilli          map[string]int64 `json:"stockMilli"`
	NextAttackUnix      int64            `json:"nextAttackUnix"`
	ScheduledDepartUnix int64            `json:"scheduledDepartUnix"`
	TargetVillageID     string           `json:"targetVillageId,omitempty"`
}

// World - полное состояние симуляции.
type World struct {
	Seed        int64               `json:"seed"`
	RngCounter  int64               `json:"rngCounter"`
	IDCounter   int64               `json:"idCounter"`
	Tick        int64               `json:"tick"`
	CreatedUnix int64               `json:"createdUnix"`
	MapSize     int                 `json:"mapSize"`
	Cells       []*Cell             `json:"cells"`
	Players     map[string]*Player  `json:"players"`
	Villages    map[string]*Village `json:"villages"`
	Camps       map[string]*Camp    `json:"camps"`
	Marches     map[string]*March   `json:"marches"`
}

// MapHalf - полуразмер карты: логические координаты клеток идут
// от -MapHalf до +MapHalf по обеим осям, центр карты - (0,0).
func (w *World) MapHalf() int {
	return w.MapSize / 2
}

// CellAt возвращает клетку по логическим координатам или nil вне карты.
func (w *World) CellAt(x, y int) *Cell {
	half := w.MapHalf()
	if x < -half || y < -half || x > half || y > half {
		return nil
	}
	return w.Cells[(y+half)*w.MapSize+(x+half)]
}

// NextRandom возвращает детерминированное псевдослучайное число [0, n).
// Генератор - splitmix64 от (Seed, RngCounter): состояние целиком в World,
// поэтому последовательность воспроизводится после загрузки снапшота.
func (w *World) NextRandom(n int64) int64 {
	if n <= 0 {
		return 0
	}
	w.RngCounter++
	z := uint64(w.Seed) + uint64(w.RngCounter)*0x9E3779B97F4A7C15
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	z = z ^ (z >> 31)
	return int64(z % uint64(n))
}
