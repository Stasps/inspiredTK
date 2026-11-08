# Inspired Kingdoms

Браузерная многопользовательская стратегия в реальном времени (аналог Travian Kingdoms). Сервер авторитетен: вся экономика, бой и время считаются в тиковой Go-симуляции, клиент - тонкий и получает состояние целиком.

## Основные возможности

- Тиковая детерминированная симуляция мира (тик 1 сек, единое серверное время)
- Генерация мира: квадратная карта с месторождениями ресурсов и варварскими лагерями (NPC-угроза, PvPvE)
- Экономика: производство ресурсов по прошедшему времени, строительство зданий с очередями, два вида складов (food / nonfood)
- Боевая система: марши армий, разрешение боёв, награды за лагеря
- Атаки варваров по расписанию (интервал в часах, из конфига)
- Персистентность: периодические снапшоты состояния + журнал боёв в JSONL
- Весь контент (ресурсы, здания, юниты, бой, мир) - в валидируемых JSON-конфигах, без хардкода в коде

## Технологический стек

- **Go 1.26** - бэкенд и симуляция
- **Архитектура**: сервер-авторитетная, слоистая (Data / Game Logic / Transport / Storage), актор-модель состояния (одна горутина на мир, без глобальных мьютексов)
- **Конфиги**: JSON-файлы, версионируемые и валидируемые при старте
- **Хранение**: файловые снапшоты (JSON) + журнал событий (JSONL); целевое решение - PostgreSQL (pgx/sqlc)
- **Транспорт (план)**: WebSocket
- **Клиент (план)**: браузерный 2D

## Установка и запуск

Требуется Go 1.26+.

```bash
# 1. Собрать
cd server
go build ./cmd/server

# 2. Запустить (каталог configs - обязательный флаг)
./server -configs configs
```

При первом старте генерируется новый мир, при последующих - состояние восстанавливается из снапшота. Остановка по Ctrl+C / SIGTERM сохраняет финальный снапшот.

Пути снапшота и журнала событий задаются в `server/configs/world.json` (`snapshotPath`, `eventLogPath`), по умолчанию - `server/data/`.

Тесты:

```bash
cd server
go test ./...
```

## Примеры использования

Сборка движка вручную (как делает `cmd/server`):

```go
cfg, err := config.Load("configs")
if err != nil {
    log.Fatal(err)
}

events, _ := persistence.OpenEventLog(cfg.World.EventLogPath)
world := sim.NewWorld(cfg, time.Now().Unix()) // или persistence.LoadSnapshot(...)
engine := sim.NewEngine(cfg, world, events)

engine.Tick(time.Now().Unix()) // один тик симуляции
```

Тиковый цикл в `server/cmd/server/main.go`:

```go
tick := time.NewTicker(time.Second)
snapTick := time.NewTicker(time.Duration(cfg.World.SnapshotIntervalSeconds) * time.Second)
for {
    select {
    case t := <-tick.C:
        engine.Tick(t.Unix())
    case <-snapTick.C:
        persistence.SaveSnapshot(cfg.World.SnapshotPath,
            persistence.NewSnapshot(engine.World, time.Now().Unix()))
    }
}
```

Добавление здания - через конфиг, не код (`server/configs/buildings.json`):

```json
{
  "id": "woodcutter",
  "levels": [
    { "level": 1, "cost": { "wood": 40, "stone": 10 }, "buildSeconds": 30, "productionPerHour": 12 }
  ]
}
```

## Структура проекта

```
inspiredTK/
├── AGENTS.md              # Правила работы агентов над проектом
├── design and architecture documents/
│   ├── concept-doc.md     # Видение игры, философия, референсы
│   ├── game-design.md     # Детальное ТЗ: механики, сезоны, эндгейм
│   ├── architecture.md    # Техническая архитектура, запреты, сетевые правила
│   ├── save-structure.md  # Формат снапшота, моменты пересчёта
│   ├── art-direction.md   # Арт-направление
│   ├── roadmap.md         # План разработки
│   └── ...                # quality-attributes, polishing, ideas
├── modules/               # Методические модули (Go, Unity, сеть, генерация изображений)
└── server/                # Бэкенд-симуляция (Go)
    ├── cmd/server/        # Точка входа: headless-сервер, тиковый цикл
    ├── configs/           # Весь игровой контент: world, resources, buildings, units, combat
    └── internal/
        ├── config/        # Слой Data: загрузка и валидация конфигов
        ├── domain/        # Типы состояния мира (целые числа, версионируемый снапшот)
        ├── sim/           # Слой Game Logic: Engine, генерация мира, бой, марши, стройка, производство, варвары
        ├── persistence/   # Слой Storage: снапшоты + журнал событий
        └── util/          # Фиксированная точность (милли-единицы)
```

Слои зависят только внутрь: `cmd -> sim -> domain/config/persistence`. Домен не зависит от транспорта и хранения - это позволяет менять клиент и БД без переписывания логики (см. `design and architecture documents/architecture.md`).
