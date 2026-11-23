package sim

import (
	"container/heap"
	"encoding/json"
	"io"
	"testing"

	"inspiredtk/server/internal/domain"
	"inspiredtk/server/internal/persistence"
)

// popOrder снимает все события с кучи и возвращает их id по порядку.
func popOrder(e *Engine) []string {
	ids := make([]string, 0, len(e.World.Events))
	for len(e.World.Events) > 0 {
		ids = append(ids, heap.Pop(e.queue()).(*domain.ScheduledEvent).ID)
	}
	return ids
}

// reloadEngine прогоняет мир через JSON, как снапшот, и собирает новый движок.
func reloadEngine(t *testing.T, e *Engine) *Engine {
	t.Helper()
	data, err := json.Marshal(e.World)
	if err != nil {
		t.Fatalf("marshal world: %v", err)
	}
	var w domain.World
	if err := json.Unmarshal(data, &w); err != nil {
		t.Fatalf("unmarshal world: %v", err)
	}
	return NewEngine(e.Cfg, &w, persistence.NewEventLog(io.Discard))
}

func TestSchedulerOrdersByFireTimeThenSeq(t *testing.T) {
	e := newTestEngine(t, testT0)
	e.World.Events = e.World.Events[:0]
	late := e.schedule(domain.EventCampAct, "c1", testT0+10)
	first := e.schedule(domain.EventCampAct, "c2", testT0+5)
	second := e.schedule(domain.EventCampAct, "c3", testT0+5)
	early := e.schedule(domain.EventCampAct, "c4", testT0+1)

	if next, ok := e.NextEventUnix(); !ok || next != testT0+1 {
		t.Fatalf("next event: got %d, %v; want %d", next, ok, testT0+1)
	}
	got := popOrder(e)
	want := []string{early, first, second, late}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pop order: got %v, want %v", got, want)
		}
	}
	if _, ok := e.NextEventUnix(); ok {
		t.Fatalf("empty scheduler must report no next event")
	}
}

func TestBuildFinishesByEventNotBefore(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerSettlement(t, e, "p1", 10, 10, testT0)
	if err := e.StartConstruction("p1", v.ID, "sawmill", testT0); err != nil {
		t.Fatalf("start construction: %v", err)
	}
	finish := v.BuildJob.FinishUnix
	if next, ok := e.NextEventUnix(); !ok || next != finish {
		t.Fatalf("nearest event must be the build finish: got %d, %v; want %d", next, ok, finish)
	}
	if fired := e.Advance(finish - 1); fired != 0 {
		t.Fatalf("no event is due before the finish, fired %d", fired)
	}
	if v.BuildJob == nil || v.Buildings["sawmill"] != 0 {
		t.Fatalf("construction finished early: %+v", v.Buildings)
	}
	if fired := e.Advance(finish); fired != 1 {
		t.Fatalf("exactly the build event must fire, fired %d", fired)
	}
	if v.BuildJob != nil || v.Buildings["sawmill"] != 1 {
		t.Fatalf("construction did not finish by event: %+v", v.BuildJob)
	}
}

// Опоздание обработки не сдвигает цепочку: обработчик получает временем
// FireUnix события, а порождённые события применяются в том же Advance.
func TestAdvanceCatchUpUsesEventTime(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := prepareWarSettlement(t, e, 0, 0, testT0)
	placeCamp(t, e, 6, 8, 3)
	id, err := e.SendMarch("p1", v.ID, domain.MarchAttack, 6, 8, 10, testT0)
	if err != nil {
		t.Fatalf("send march: %v", err)
	}
	arrive := e.World.Marches[id].ArriveUnix
	travel := arrive - testT0

	// Сервер «проспал» прибытие на 100 секунд.
	e.Advance(arrive + 100)
	if len(e.World.Marches) != 1 {
		t.Fatalf("want one return march, got %d", len(e.World.Marches))
	}
	for _, m := range e.World.Marches {
		if m.DepartUnix != arrive || m.ArriveUnix != arrive+travel {
			t.Fatalf("return march must start at the arrival time %d: %+v", arrive, m)
		}
	}

	// Простой дольше всей цепочки: бой и возврат применяются одним вызовом.
	e2 := newTestEngine(t, testT0)
	v2 := prepareWarSettlement(t, e2, 0, 0, testT0)
	placeCamp(t, e2, 6, 8, 3)
	if _, err := e2.SendMarch("p1", v2.ID, domain.MarchAttack, 6, 8, 10, testT0); err != nil {
		t.Fatalf("send march: %v", err)
	}
	e2.Advance(arrive + travel + 1000)
	if len(e2.World.Marches) != 0 || v2.StationedInfantry != 16 {
		t.Fatalf("chain must finish in one catch-up: %d marches, %d infantry", len(e2.World.Marches), v2.StationedInfantry)
	}
}

// Одновременные прибытия обрабатываются в порядке постановки событий.
func TestSimultaneousArrivalsResolveInScheduleOrder(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := prepareWarSettlement(t, e, 0, 0, testT0)
	camp := placeCamp(t, e, 6, 8, 3)
	// Первый отряд слишком слаб и гибнет, второй добивает ослабленный лагерь.
	// При обратном порядке первый пришёл бы к пустому лагерю и выжил.
	first, err := e.SendMarch("p1", v.ID, domain.MarchAttack, 6, 8, 1, testT0)
	if err != nil {
		t.Fatalf("send first march: %v", err)
	}
	if _, err := e.SendMarch("p1", v.ID, domain.MarchAttack, 6, 8, 10, testT0); err != nil {
		t.Fatalf("send second march: %v", err)
	}
	e.Advance(e.World.Marches[first].ArriveUnix)
	if camp.Infantry != 0 {
		t.Fatalf("camp must be wiped by the second march: %d", camp.Infantry)
	}
	returning := 0
	for _, m := range e.World.Marches {
		returning += m.Infantry
	}
	// Первый отряд (1 боец) погиб о целый лагерь, значит шёл первым.
	if returning >= 10 {
		t.Fatalf("first march must have fought the full camp, %d returning", returning)
	}
}

func TestCancelledMarchEventIsDropped(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := prepareWarSettlement(t, e, 0, 0, testT0)
	camp := placeCamp(t, e, 6, 8, 3)
	id, err := e.SendMarch("p1", v.ID, domain.MarchAttack, 6, 8, 5, testT0)
	if err != nil {
		t.Fatalf("send march: %v", err)
	}
	arrive := e.World.Marches[id].ArriveUnix
	if err := e.CancelMarch("p1", id, testT0+100); err != nil {
		t.Fatalf("cancel march: %v", err)
	}
	// Позже исходного прибытия: отменённое событие не должно дать бой,
	// а возврат должен зачислиться ровно один раз.
	e.Advance(arrive + 1)
	if camp.Infantry != 3 {
		t.Fatalf("cancelled march must not fight: camp %d", camp.Infantry)
	}
	if v.StationedInfantry != 20 || len(e.World.Marches) != 0 {
		t.Fatalf("troops must return once: %d infantry, %d marches", v.StationedInfantry, len(e.World.Marches))
	}
	if len(e.World.Events) != 0 {
		for _, ev := range e.World.Events {
			if ev.Kind == domain.EventMarchArrive {
				t.Fatalf("stale march event left in the scheduler: %+v", ev)
			}
		}
	}
}

// Событие завершённой досрочно стройки не должно завершить следующую.
func TestStaleBuildEventDoesNotFinishNextJob(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerSettlement(t, e, "p1", 10, 10, testT0)
	if err := e.StartConstruction("p1", v.ID, "sawmill", testT0); err != nil {
		t.Fatalf("start construction: %v", err)
	}
	staleFinish := v.BuildJob.FinishUnix
	if err := e.InstantComplete("p1", v.ID, testT0); err != nil {
		t.Fatalf("instant complete: %v", err)
	}
	// Вторая стройка стартует за секунду до старого дедлайна.
	if err := e.StartConstruction("p1", v.ID, "farm", staleFinish-1); err != nil {
		t.Fatalf("start second construction: %v", err)
	}
	finish := v.BuildJob.FinishUnix
	if finish <= staleFinish {
		t.Fatalf("test setup: second job must finish after the stale event (%d <= %d)", finish, staleFinish)
	}
	e.Advance(staleFinish)
	if v.BuildJob == nil || v.Buildings["farm"] != 0 {
		t.Fatalf("stale event finished the next job early")
	}
	e.Advance(finish)
	if v.BuildJob != nil || v.Buildings["farm"] != 1 {
		t.Fatalf("second job did not finish: %+v", v.BuildJob)
	}
}

func TestSchedulerSurvivesSnapshotRoundTrip(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := prepareWarSettlement(t, e, 0, 0, testT0)
	// Тестовый лагерь создан в обход генерации мира: ставим его событие
	// сами, иначе его досоздаст восстановление и счёт событий изменится.
	camp := placeCamp(t, e, 6, 8, 3)
	camp.NextAttackUnix = testT0 + 10*24*3600
	e.armCamp(camp, testT0)
	if err := e.StartConstruction("p1", v.ID, "sawmill", testT0); err != nil {
		t.Fatalf("start construction: %v", err)
	}
	finish := v.BuildJob.FinishUnix
	id, err := e.SendMarch("p1", v.ID, domain.MarchAttack, 6, 8, 10, testT0)
	if err != nil {
		t.Fatalf("send march: %v", err)
	}
	arrive := e.World.Marches[id].ArriveUnix
	eventsBefore, seqBefore := len(e.World.Events), e.World.EventSeq

	loaded := reloadEngine(t, e)
	if len(loaded.World.Events) != eventsBefore || loaded.World.EventSeq != seqBefore {
		t.Fatalf("restore must not add or lose events: %d -> %d, seq %d -> %d",
			eventsBefore, len(loaded.World.Events), seqBefore, loaded.World.EventSeq)
	}
	lv := loaded.World.Settlements[v.ID]
	loaded.Advance(max(finish, arrive))
	if lv.BuildJob != nil || lv.Buildings["sawmill"] != 1 {
		t.Fatalf("restored build event did not fire: %+v", lv.BuildJob)
	}
	if camp := loaded.World.Camps["tc-6-8"]; camp.Infantry != 0 {
		t.Fatalf("restored march event did not fire: camp %d", camp.Infantry)
	}
}

// Снапшот старого формата (без events и eventId): события досоздаются из
// дедлайнов сущностей.
func TestLegacySnapshotGetsEventsFromDeadlines(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := prepareWarSettlement(t, e, 0, 0, testT0)
	camp := placeCamp(t, e, 6, 8, 3)
	if err := e.StartConstruction("p1", v.ID, "sawmill", testT0); err != nil {
		t.Fatalf("start construction: %v", err)
	}
	finish := v.BuildJob.FinishUnix
	id, err := e.SendMarch("p1", v.ID, domain.MarchAttack, 6, 8, 10, testT0)
	if err != nil {
		t.Fatalf("send march: %v", err)
	}
	arrive := e.World.Marches[id].ArriveUnix

	// Имитация старого формата: планировщика в состоянии нет.
	e.World.Events = nil
	e.World.EventSeq = 0
	v.BuildJob.EventID = ""
	e.World.Marches[id].EventID = ""
	for _, c := range e.World.Camps {
		c.EventID = ""
	}

	loaded := reloadEngine(t, e)
	armed := 0
	for _, c := range loaded.World.Camps {
		if c.EventID != "" {
			armed++
		}
	}
	// Тестовый лагерь и сгенерированные - все с войсками, все должны встать в очередь.
	if armed != len(loaded.World.Camps) {
		t.Fatalf("camps armed: %d of %d", armed, len(loaded.World.Camps))
	}
	lv := loaded.World.Settlements[v.ID]
	loaded.Advance(max(finish, arrive))
	if lv.BuildJob != nil || lv.Buildings["sawmill"] != 1 {
		t.Fatalf("legacy build job did not finish: %+v", lv.BuildJob)
	}
	if got := loaded.World.Camps[camp.ID].StockMilli; len(got) != 0 {
		t.Fatalf("legacy march did not arrive: camp stock %v", got)
	}
}

func TestSkipTimeShiftsScheduledEvents(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerSettlement(t, e, "p1", 10, 10, testT0)
	if err := e.StartConstruction("p1", v.ID, "sawmill", testT0); err != nil {
		t.Fatalf("start construction: %v", err)
	}
	remaining := v.BuildJob.FinishUnix - testT0
	if err := e.SkipTime(remaining); err != nil {
		t.Fatalf("skip time: %v", err)
	}
	if next, ok := e.NextEventUnix(); !ok || next != testT0 {
		t.Fatalf("build event must move to now: got %d, %v; want %d", next, ok, testT0)
	}
	e.Advance(testT0)
	if v.BuildJob != nil || v.Buildings["sawmill"] != 1 {
		t.Fatalf("skipped construction did not finish: %+v", v.BuildJob)
	}
}

// Лагерь без войск не держит события; вернувшиеся выжившие ставят его снова.
func TestCampIdlesWithoutArmyAndRearmsOnReturn(t *testing.T) {
	e := newTestEngine(t, testT0)
	v := newPlayerSettlement(t, e, "p1", 0, 0, testT0)
	camp := placeCamp(t, e, 6, 8, 30)
	camp.NextAttackUnix = testT0
	camp.TargetSettlementID = v.ID
	camp.ScheduledDepartUnix = testT0
	e.armCamp(camp, testT0)

	e.Advance(testT0)
	if camp.Infantry != 0 || camp.EventID != "" {
		t.Fatalf("camp with its army away must idle: infantry %d, event %q", camp.Infantry, camp.EventID)
	}
	var march *domain.March
	for _, m := range e.World.Marches {
		if m.OriginCampID == camp.ID {
			march = m
		}
	}
	if march == nil {
		t.Fatalf("camp did not launch its attack")
	}
	// Поселение без гарнизона: все 30 выживают и возвращаются в лагерь.
	e.Advance(march.ArriveUnix)
	if camp.Infantry != 30 {
		t.Fatalf("survivors must return to the camp: %d", camp.Infantry)
	}
	if camp.EventID == "" {
		t.Fatalf("camp with returned army must be scheduled again")
	}
	if next, ok := e.NextEventUnix(); !ok || next < march.ArriveUnix {
		t.Fatalf("camp event must not be scheduled in the past: %d, %v", next, ok)
	}
}
