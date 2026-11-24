package sim

import (
	"container/heap"
	"fmt"
	"sort"
	"strconv"

	"inspiredtk/server/internal/domain"
)

// Планировщик событий: min-heap по (FireUnix, Seq) поверх World.Events
// (documentation/architecture.md, «Конкурентная модель»). Тика нет: «жёсткие»
// дедлайны (стройка, прибытие марша, действия лагеря) лежат в куче, а
// владелец Engine вызывает Advance по таймеру ближайшего события.
//
// Отменённое событие из кучи не удаляется: сущность хранит id своего
// актуального события, и обработчик отбрасывает событие с другим id.

// eventEntityType - тип сущности, которой адресован каждый вид события.
// Заодно это полный список известных видов.
var eventEntityType = map[domain.EventKind]string{
	domain.EventBuildFinish: "settlement",
	domain.EventMarchArrive: "march",
	domain.EventCampAct:     "camp",
}

// eventQueue реализует heap.Interface поверх среза событий мира.
type eventQueue []*domain.ScheduledEvent

func (q eventQueue) Len() int { return len(q) }

func (q eventQueue) Less(i, j int) bool {
	if q[i].FireUnix != q[j].FireUnix {
		return q[i].FireUnix < q[j].FireUnix
	}
	return q[i].Seq < q[j].Seq
}

func (q eventQueue) Swap(i, j int) { q[i], q[j] = q[j], q[i] }

func (q *eventQueue) Push(x any) { *q = append(*q, x.(*domain.ScheduledEvent)) }

func (q *eventQueue) Pop() any {
	old := *q
	n := len(old)
	ev := old[n-1]
	old[n-1] = nil
	*q = old[:n-1]
	return ev
}

func (e *Engine) queue() *eventQueue {
	return (*eventQueue)(&e.World.Events)
}

// schedule ставит событие в планировщик.
// Parameters:
//   - kind: вид события
//   - entityID: сущность, которой оно адресовано
//   - fireUnix: серверное время срабатывания
//
// Returns: id события; сущность хранит его, чтобы отличать актуальное
// событие от отменённого.
func (e *Engine) schedule(kind domain.EventKind, entityID string, fireUnix int64) string {
	entityType, ok := eventEntityType[kind]
	if !ok {
		panic(fmt.Errorf("scheduler: unknown event kind %q for entity %q", kind, entityID))
	}
	e.World.EventSeq++
	ev := &domain.ScheduledEvent{
		ID:         "e" + strconv.FormatInt(e.World.EventSeq, 10),
		Seq:        e.World.EventSeq,
		Kind:       kind,
		EntityType: entityType,
		EntityID:   entityID,
		FireUnix:   fireUnix,
	}
	heap.Push(e.queue(), ev)
	return ev.ID
}

// NextEventUnix возвращает время ближайшего события планировщика.
//
// Returns: время срабатывания и true; (0, false), если событий нет.
func (e *Engine) NextEventUnix() (int64, bool) {
	if len(e.World.Events) == 0 {
		return 0, false
	}
	return e.World.Events[0].FireUnix, true
}

// Advance применяет все события с FireUnix <= now по порядку (FireUnix, Seq).
// Обработчик получает игровым временем FireUnix события, а не now: опоздание
// обработки (простой сервера, занятый цикл) не сдвигает цепочки событий.
// События, порождённые обработчиками и уже просроченные, применяются в этом
// же вызове. Вызывать перед каждой командой и по таймеру ближайшего события.
// Parameters:
//   - now: серверное время (единственный источник времени)
//
// Returns: число снятых с кучи событий, включая отменённые.
func (e *Engine) Advance(now int64) int {
	fired := 0
	for len(e.World.Events) > 0 && e.World.Events[0].FireUnix <= now {
		ev := heap.Pop(e.queue()).(*domain.ScheduledEvent)
		e.dispatch(ev)
		fired++
	}
	return fired
}

// dispatch выполняет событие, если оно ещё актуально для своей сущности.
func (e *Engine) dispatch(ev *domain.ScheduledEvent) {
	switch ev.Kind {
	case domain.EventBuildFinish:
		v := e.World.Settlements[ev.EntityID]
		if v == nil || v.BuildJob == nil || v.BuildJob.EventID != ev.ID {
			return
		}
		e.completeConstruction(v)
	case domain.EventMarchArrive:
		m := e.World.Marches[ev.EntityID]
		if m == nil || m.EventID != ev.ID {
			return
		}
		e.resolveMarch(m, ev.FireUnix)
		delete(e.World.Marches, m.ID)
	case domain.EventCampAct:
		camp := e.World.Camps[ev.EntityID]
		if camp == nil || camp.EventID != ev.ID {
			return
		}
		camp.EventID = ""
		e.actCamp(camp, ev.FireUnix)
	default:
		panic(fmt.Errorf("scheduler: unknown event kind %q (event %s, entity %q)", ev.Kind, ev.ID, ev.EntityID))
	}
}

// restoreSchedule восстанавливает кучу после загрузки мира и досоздаёт
// события для дедлайнов, у которых их нет: новый мир, снапшот старого
// формата (без events) или сущности, добавленные в обход команд.
// Порядок обхода отсортирован, поэтому id новых событий детерминированы.
func (e *Engine) restoreSchedule() {
	pending := make(map[string]bool, len(e.World.Events))
	for i, ev := range e.World.Events {
		if ev == nil {
			panic(fmt.Errorf("scheduler: snapshot has an empty event record at index %d", i))
		}
		if _, ok := eventEntityType[ev.Kind]; !ok {
			panic(fmt.Errorf("scheduler: snapshot has unknown event kind %q (event %s, entity %q)", ev.Kind, ev.ID, ev.EntityID))
		}
		pending[ev.ID] = true
	}
	heap.Init(e.queue())

	for _, id := range sortedKeys(e.World.Settlements) {
		v := e.World.Settlements[id]
		if job := v.BuildJob; job != nil && !pending[job.EventID] {
			job.EventID = e.schedule(domain.EventBuildFinish, v.ID, job.FinishUnix)
		}
	}

	orphans := make([]*domain.March, 0)
	for _, m := range e.World.Marches {
		if !pending[m.EventID] {
			orphans = append(orphans, m)
		}
	}
	sort.Slice(orphans, func(i, j int) bool {
		if orphans[i].ArriveUnix != orphans[j].ArriveUnix {
			return orphans[i].ArriveUnix < orphans[j].ArriveUnix
		}
		return orphans[i].ID < orphans[j].ID
	})
	for _, m := range orphans {
		m.EventID = e.schedule(domain.EventMarchArrive, m.ID, m.ArriveUnix)
	}

	for _, id := range sortedKeys(e.World.Camps) {
		camp := e.World.Camps[id]
		if !pending[camp.EventID] {
			e.armCamp(camp, 0)
		}
	}
}

// shiftSchedule сдвигает время всех событий (debug-перемотка времени) и
// восстанавливает инвариант кучи.
func (e *Engine) shiftSchedule(shift func(int64) int64) {
	for _, ev := range e.World.Events {
		ev.FireUnix = shift(ev.FireUnix)
	}
	heap.Init(e.queue())
}
