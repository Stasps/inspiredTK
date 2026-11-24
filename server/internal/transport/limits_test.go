package transport

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// fakeClock - управляемое время для token bucket.
type fakeClock struct{ nanos atomic.Int64 }

func newFakeClock() *fakeClock {
	c := &fakeClock{}
	c.nanos.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return c
}

func (c *fakeClock) now() time.Time          { return time.Unix(0, c.nanos.Load()) }
func (c *fakeClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

// startLimitedServer поднимает сервер с заданными пределами и замороженными
// часами: жетоны не восполняются, пока тест не сдвинет время.
func startLimitedServer(t *testing.T, edit func(*Limits)) (*Server, string, *fakeClock) {
	t.Helper()
	l := DefaultLimits()
	l.OutboxSize = 64
	edit(&l)
	srv := newServer(l)
	clock := newFakeClock()
	srv.now = clock.now
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWebSocket))
	t.Cleanup(ts.Close)
	return srv, "ws" + strings.TrimPrefix(ts.URL, "http"), clock
}

func noEvent(t *testing.T, srv *Server) {
	t.Helper()
	select {
	case ev := <-srv.Events():
		t.Fatalf("unexpected event %+v", ev)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestTokenBucket(t *testing.T) {
	clock := newFakeClock()
	l := &playerLimiter{tokens: 10, last: clock.now()}
	for i := range 10 {
		if !l.allow(clock.now(), 10, 10) {
			t.Fatalf("command %d denied within burst", i+1)
		}
	}
	if l.allow(clock.now(), 10, 10) {
		t.Fatal("11th command allowed without refill")
	}
	clock.advance(100 * time.Millisecond)
	if !l.allow(clock.now(), 10, 10) {
		t.Fatal("token not refilled after 100ms at 10/s")
	}
	if l.allow(clock.now(), 10, 10) {
		t.Fatal("more than one token refilled in 100ms")
	}
	// Долгий простой не копит больше запаса.
	clock.advance(time.Hour)
	allowed := 0
	for l.allow(clock.now(), 10, 10) {
		allowed++
	}
	if allowed != 10 {
		t.Fatalf("after idle allowed %d, want burst 10", allowed)
	}
}

// Не больше MaxQueuedPerPlayer команд игрока в очереди; место освобождает
// Taken, когда симуляция забирает команду.
func TestPlayerQueueLimit(t *testing.T) {
	srv, url, _ := startLimitedServer(t, func(l *Limits) { l.MaxQueuedPerPlayer = 4 })
	c := dial(t, url+"?player=alice")
	nextEvent(t, srv)
	for range 4 {
		sendCommand(t, c, "noop", nil)
	}
	fifth := sendCommand(t, c, "noop", nil)
	readError(t, c, CodeServerBusy, fifth)

	ev := nextEvent(t, srv)
	ev.Taken()
	sendCommand(t, c, "noop", nil)
	for range 4 {
		if got := nextEvent(t, srv); got.Kind != EventCommand {
			t.Fatalf("event %+v, want command", got)
		}
	}
	noEvent(t, srv)
}

// Лимит очереди и token bucket общие для вкладок игрока и не задевают
// других игроков.
func TestLimitsSharedAcrossPlayerSessions(t *testing.T) {
	srv, url, _ := startLimitedServer(t, func(l *Limits) { l.CommandBurst = 5; l.MaxQueuedPerPlayer = 4 })
	a1, a2, b := dial(t, url+"?player=alice"), dial(t, url+"?player=alice"), dial(t, url+"?player=bob")
	for range 3 {
		nextEvent(t, srv)
	}
	// Команды разбираются в разных горутинах чтения: ждём прихода каждой
	// пары, чтобы порядок был определён. Taken не вызывается - места заняты.
	for _, c := range []*websocket.Conn{a1, a2} {
		sendCommand(t, c, "noop", nil)
		sendCommand(t, c, "noop", nil)
		nextEvent(t, srv)
		nextEvent(t, srv)
	}
	busy := sendCommand(t, a2, "noop", nil)
	readError(t, a2, CodeServerBusy, busy)
	limited := sendCommand(t, a1, "noop", nil) // шестая команда alice при запасе 5
	readError(t, a1, CodeRateLimited, limited)

	sendCommand(t, b, "noop", nil)
	if ev := nextEvent(t, srv); ev.Kind != EventCommand || ev.Session.PlayerID != "bob" {
		t.Fatalf("event %+v, want bob's command", ev)
	}
	noEvent(t, srv)
}

func (s *Session) isClosed() bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

// Превышение частоты: отказ rate_limited с cmd_id; после MaxRateStrikes
// отказов подряд соединение рвётся. Восполненный жетон сбрасывает счёт.
func TestRateLimitThenDisconnect(t *testing.T) {
	srv, url, clock := startLimitedServer(t, func(l *Limits) {
		l.CommandBurst = 3
		l.MaxRateStrikes = 3
		l.MaxQueuedPerPlayer = 100
	})
	c := dial(t, url+"?player=alice")
	join := nextEvent(t, srv)
	for range 3 {
		sendCommand(t, c, "noop", nil)
	}
	for range 2 {
		id := sendCommand(t, c, "noop", nil)
		readError(t, c, CodeRateLimited, id)
	}
	clock.advance(100 * time.Millisecond) // один жетон: счёт отказов сбрасывается
	sendCommand(t, c, "noop", nil)
	for range 2 {
		id := sendCommand(t, c, "noop", nil)
		readError(t, c, CodeRateLimited, id)
	}
	if join.Session.isClosed() {
		t.Fatal("session closed before MaxRateStrikes in a row")
	}
	last := sendCommand(t, c, "noop", nil)
	readError(t, c, CodeRateLimited, last)
	_ = c.SetReadDeadline(time.Now().Add(testWait))
	if _, _, err := c.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("read after strikes: %v, want close 1008", err)
	}
	for range 4 {
		if ev := nextEvent(t, srv); ev.Kind != EventCommand {
			t.Fatalf("event %+v, want 4 accepted commands", ev)
		}
	}
	if ev := nextEvent(t, srv); ev.Kind != EventLeave {
		t.Fatalf("event %+v, want leave", ev)
	}
}

// Отказ из-за полной общей очереди возвращает место игрока.
func TestQueueFullReleasesPlayerSlot(t *testing.T) {
	srv, url, _ := startLimitedServer(t, func(l *Limits) { l.QueueSize = 2 })
	c := dial(t, url+"?player=alice")
	join := nextEvent(t, srv)
	sendCommand(t, c, "noop", nil)
	sendCommand(t, c, "noop", nil)
	full := sendCommand(t, c, "noop", nil)
	readError(t, c, CodeServerBusy, full)
	join.Session.limiter.mu.Lock()
	queued := join.Session.limiter.queued
	join.Session.limiter.mu.Unlock()
	if queued != 2 {
		t.Fatalf("queued %d, want 2", queued)
	}
}
