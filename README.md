# Inspired Kingdoms

Браузерная многопользовательская стратегия в реальном времени (аналог Travian Kingdoms). Сервер авторитетен: вся экономика, бой и время считаются в Go-симуляции с ленивыми вычислениями ресурсов и планировщиком событий (тика как такового нет). Клиент — тонкий и получает состояние целиком.

## Основные возможности

- **Ленивые вычисления ресурсов** (Compute on Read): ресурсы вычисляются по формуле `rate * (now - last_update_at)` в момент чтения, а не тикают. «Жёсткие» события (стройка, марши, найм) обрабатываются планировщиком по серверному времени
- Генерация мира: квадратная карта с месторождениями ресурсов и варварскими лагерями (NPC-угроза, PvPvE)
- Экономика: производство ресурсов по прошедшему времени, строительство зданий с очередями, два вида складов (food / nonfood)
- Боевая система: марши армий, разрешение боёв, награды за лагеря
- Атаки варваров по расписанию (интервал в часах, из конфига)
- Персистентность: периодические снапшоты состояния + append-only журнал событий (JSONL) с `last_event_id` для восстановления
- Весь контент (ресурсы, здания, юниты, бой, мир) - в валидируемых JSON-конфигах, без хардкода в коде
- Actor-модель состояния: одна сущность — одна горутина с почтовым ящиком, без глобальных мьютексов

## Технологический стек

- **Go 1.26** - бэкенд и симуляция
- **Архитектура**: сервер-авторитетная, слоистая (Data / Game Logic / Transport / Storage), 
  событийно-ориентированная (без тика: планировщик событий min-heap по `fire_at` + 
  ленивые вычисления ресурсов Compute on Read), Actor-модель (одна горутина на сущность), 
  без глобальных мьютексов
- **Конфиги**: JSON-файлы, версионируемые и валидируемые при старте
- **Хранение**: файловые снапшоты (JSON, с `version`, `snapshot_at`, `last_event_id`) + append-only журнал      событий (JSONL, с `event_id` и `idempotency_key`); целевое решение - PostgreSQL (pgx/sqlc)
- **Транспорт**: WebSocket (`gorilla/websocket`) — реализован
- **Клиент**: debug HTML (`client/index.html`) — реализован, планируется полноценный 2D

## Установка и запуск

Требуется Go 1.26+.

```bash
# 1. Установить зависимости
cd server
go get github.com/gorilla/websocket

# 2. Собрать
go build ./cmd/server

# 3. Запустить (каталог configs - обязательный флаг)
./server -configs configs

# 4. Открыть клиент
# Открыть client/index.html в браузере
```

При первом старте генерируется новый мир, при последующих — состояние восстанавливается из снапшота + реплей событий из журнала с `id > last_event_id`. Планировщик при старте восстанавливает min-heap из таблицы/файла `scheduled_event` и пакетно обрабатывает события, чьё `fire_at` прошло во время простоя. Остановка по Ctrl+C / SIGTERM сохраняет финальный снапшот.

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

// Ленивый пересчёт ресурсов — при чтении состояния сущности:
v := world.Village(id)
v.RefreshResources(time.Now().Unix()) // rate * (now - v.LastUpdateAt)

// «Жёсткие» события — через планировщик:
engine.Scheduler.FireDue(time.Now().Unix()) // обработать все события с fire_at <= now
```

Цикл в `server/cmd/server/main.go`:

```go
snapTick := time.NewTicker(time.Duration(cfg.World.SnapshotIntervalSeconds) * time.Second)
schedulerTick := time.NewTicker(500 * time.Millisecond) // проверка просроченных событий
for {
    select {
    case <-schedulerTick.C:
        engine.Scheduler.FireDue(time.Now().Unix()) // пакетно: все события с fire_at <= now
    case <-snapTick.C:
        persistence.SaveSnapshot(cfg.World.SnapshotPath,
            persistence.NewSnapshot(engine.World, time.Now().Unix())) // пишет last_event_id
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
├── client/                # Debug HTML-клиент (WebSocket)
├── documentation/         # Общая документация и планы
│   ├── concept-doc.md     # Видение игры, философия, референсы
│   ├── game-design.md     # Детальное ТЗ: механики, сезоны, эндгейм
│   ├── architecture.md    # Техническая архитектура, запреты, сетевые правила
│   ├── save-structure.md  # Формат снапшота, моменты пересчёта
│   ├── art-direction.md   # Арт-направление
│   ├── roadmap.md         # План разработки
│   └── ...                # quality-attributes, polishing, ideas
├── modules/               # Методические модули (Go, сеть, генерация изображений)
├── ref-balance/           # Эталонные данные баланса из Travian (CSV)
└── server/                # Бэкенд-симуляция (Go)
    ├── cmd/server/        # Точка входа: HTTP/WebSocket + планировщик + снапшоты
    ├── configs/           # Весь игровой контент: world, resources, buildings, units, combat
    ├── data/              # Снапшоты и журнал событий (runtime)
    └── internal/
        ├── config/        # Слой Data: загрузка и валидация конфигов
        ├── domain/        # Типы состояния мира
        ├── sim/           # Слой Game Logic: Engine, планировщик, бой, марши
        ├── persistence/   # Слой Storage: снапшоты + журнал событий
        ├── transport/     # Слой Transport: WebSocket
        └── util/          # Фиксированная точность (милли-единицы)
```

Слои зависят только внутрь: `cmd -> sim -> domain/config/persistence`. Домен не зависит от транспорта и хранения - это позволяет менять клиент и БД без переписывания логики (см. `documentation/architecture.md`).

## Источник баланса

Числовые значения (стоимость зданий, характеристики юнитов, скорость производства, время найма) берутся из **TravianZ (T3.6)** как справочный материал. Код PHP не копируется: все структуры данных и логика на Go — авторские. Справочные материалы лежат в `ref-balance/` и не являются частью проекта. Фракции и визуал — собственные.
