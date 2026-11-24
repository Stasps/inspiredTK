package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testWait = 2 * time.Second

// fakeConn - соединение без сети. Запись блокируется, пока открыт gate
// (nil - не блокируется); одновременные писатели считаются.
type fakeConn struct {
	gate      chan struct{}
	closed    chan struct{}
	closeOnce sync.Once

	mu      sync.Mutex
	written [][]byte
	types   []int

	writers    atomic.Int32
	maxWriters atomic.Int32
}

func newFakeConn(blocking bool) *fakeConn {
	c := &fakeConn{closed: make(chan struct{})}
	if blocking {
		c.gate = make(chan struct{})
	}
	return c
}

func (c *fakeConn) ReadMessage() (int, []byte, error) {
	<-c.closed
	return 0, nil, errors.New("closed")
}

func (c *fakeConn) WriteMessage(messageType int, data []byte) error {
	n := c.writers.Add(1)
	defer c.writers.Add(-1)
	for {
		m := c.maxWriters.Load()
		if n <= m || c.maxWriters.CompareAndSwap(m, n) {
			break
		}
	}
	if c.gate != nil {
		select {
		case <-c.gate:
		case <-c.closed:
			return errors.New("closed")
		}
	}
	select {
	case <-c.closed:
		return errors.New("closed")
	default:
	}
	c.mu.Lock()
	c.written = append(c.written, data)
	c.types = append(c.types, messageType)
	c.mu.Unlock()
	return nil
}

func (c *fakeConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

func (c *fakeConn) snapshot() ([][]byte, []int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([][]byte(nil), c.written...), append([]int(nil), c.types...)
}

func waitClosed(t *testing.T, s *Session) {
	t.Helper()
	select {
	case <-s.Done():
	case <-time.After(testWait):
		t.Fatal("session was not closed")
	}
}

func waitWritten(t *testing.T, c *fakeConn, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for time.Now().Before(deadline) {
		if w, _ := c.snapshot(); len(w) >= n {
			return w
		}
		time.Sleep(time.Millisecond)
	}
	w, _ := c.snapshot()
	t.Fatalf("written %d messages, want %d", len(w), n)
	return nil
}

// Медленный клиент не блокирует отправителя: при переполнении очереди
// Send сразу возвращает false и сессия закрывается.
func TestSendDoesNotBlockOnSlowClient(t *testing.T) {
	conn := newFakeConn(true)
	s := newSession(1, "p1", conn, 4)

	done := make(chan int)
	go func() {
		accepted := 0
		for range 100 {
			if s.Send([]byte("x")) {
				accepted++
			}
		}
		done <- accepted
	}()
	select {
	case accepted := <-done:
		// В очередь влезает outboxSize плюс, возможно, одно сообщение,
		// которое писатель уже забрал и держит в WriteMessage.
		if accepted < 4 || accepted > 5 {
			t.Fatalf("accepted %d messages, want 4-5", accepted)
		}
	case <-time.After(testWait):
		t.Fatal("Send blocked on a slow client")
	}
	waitClosed(t, s)
	if s.Send([]byte("late")) {
		t.Fatal("Send on a closed session returned true")
	}
}

// Пишет в соединение только одна горутина, порядок сообщений сохраняется,
// даже когда Send вызывают несколько горутин (симуляция и чтение).
func TestSingleWriterKeepsOrder(t *testing.T) {
	conn := newFakeConn(false)
	s := newSession(1, "p1", conn, 1000)
	var wg sync.WaitGroup
	for g := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 100 {
				s.Send([]byte{byte('a' + g), byte(i)})
			}
		}()
	}
	wg.Wait()
	written := waitWritten(t, conn, 400)
	if m := conn.maxWriters.Load(); m != 1 {
		t.Fatalf("max concurrent writers %d, want 1", m)
	}
	next := map[byte]byte{}
	for _, w := range written {
		if w[1] != next[w[0]] {
			t.Fatalf("goroutine %c: message %d out of order, want %d", w[0], w[1], next[w[0]])
		}
		next[w[0]]++
	}
	s.Close()
}

// SendAndClose доставляет последнее сообщение, кадр закрытия и закрывает сессию.
func TestSendAndClose(t *testing.T) {
	conn := newFakeConn(false)
	s := newSession(1, "p1", conn, 4)
	s.SendAndClose([]byte("bye"))
	waitClosed(t, s)
	written, types := conn.snapshot()
	if len(written) != 2 || string(written[0]) != "bye" || types[1] != websocket.CloseMessage {
		t.Fatalf("written %q types %v, want message and close frame", written, types)
	}
}

// Close идемпотентен и безопасен одновременно с Send.
func TestCloseConcurrentWithSend(t *testing.T) {
	conn := newFakeConn(false)
	s := newSession(1, "p1", conn, 4)
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				s.Send([]byte("x"))
			}
			s.Close()
		}()
	}
	wg.Wait()
	waitClosed(t, s)
}

// Hub рассылает всем сессиям, снятая сессия больше ничего не получает.
func TestHubBroadcastAndRemove(t *testing.T) {
	h := NewHub()
	c1, c2 := newFakeConn(false), newFakeConn(false)
	s1, s2 := newSession(1, "p1", c1, 8), newSession(2, "p2", c2, 8)
	defer s1.Close()
	defer s2.Close()
	h.Add(s2)
	h.Add(s1)
	if got := h.Sessions(); len(got) != 2 || got[0] != s1 || got[1] != s2 {
		t.Fatalf("Sessions not ordered by ID")
	}
	h.Broadcast(MessageTypeState, map[string]int{"n": 1})
	waitWritten(t, c1, 1)
	waitWritten(t, c2, 1)

	h.Remove(s2)
	h.Broadcast(MessageTypeState, map[string]int{"n": 2})
	h.Send(s1, MessageTypeError, "only s1")
	waitWritten(t, c1, 3)
	time.Sleep(20 * time.Millisecond)
	if w, _ := c2.snapshot(); len(w) != 1 {
		t.Fatalf("removed session got %d messages, want 1", len(w))
	}
}

// --- Сквозные проверки через настоящий WebSocket ---

func startServer(t *testing.T, queueSize int) (*Server, string) {
	t.Helper()
	srv := newServer(queueSize, 8)
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWebSocket))
	t.Cleanup(ts.Close)
	return srv, "ws" + strings.TrimPrefix(ts.URL, "http")
}

func dial(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	c, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func nextEvent(t *testing.T, srv *Server) Event {
	t.Helper()
	select {
	case ev := <-srv.Events():
		return ev
	case <-time.After(testWait):
		t.Fatal("no event from server")
		return Event{}
	}
}

func sendCommand(t *testing.T, c *websocket.Conn, name string, data any) {
	t.Helper()
	raw, _ := json.Marshal(data)
	if err := c.WriteJSON(Message{Type: MessageTypeCommand, Cmd: name, Data: raw}); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func readMessage(t *testing.T, c *websocket.Conn) Message {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(testWait))
	var msg Message
	if err := c.ReadJSON(&msg); err != nil {
		t.Fatalf("read: %v", err)
	}
	return msg
}

// Порядок событий сессии: Join, команды, Leave; ответ идёт через сессию.
func TestServerSessionLifecycle(t *testing.T) {
	srv, url := startServer(t, 16)
	c := dial(t, url+"?player=alice")

	join := nextEvent(t, srv)
	if join.Kind != EventJoin || join.Session == nil || join.Session.PlayerID != "alice" {
		t.Fatalf("first event %+v, want join", join)
	}
	sendCommand(t, c, "upgrade_building", map[string]string{"building": "main"})
	cmd := nextEvent(t, srv)
	if cmd.Kind != EventCommand || cmd.Session != join.Session || cmd.Command.Name != "upgrade_building" || cmd.Command.Data["building"] != "main" {
		t.Fatalf("command event %+v", cmd)
	}

	join.Session.Send(Encode(MessageTypeState, map[string]int{"n": 1}))
	if msg := readMessage(t, c); msg.Type != MessageTypeState {
		t.Fatalf("got %s, want state", msg.Type)
	}

	c.Close()
	leave := nextEvent(t, srv)
	if leave.Kind != EventLeave || leave.Session != join.Session {
		t.Fatalf("event %+v, want leave of the same session", leave)
	}
}

// Переполненная очередь команд не блокирует чтение, отказ получает
// отправитель.
func TestServerQueueFullRejectsToSender(t *testing.T) {
	srv, url := startServer(t, 2)
	c := dial(t, url+"?player=p1")
	nextEvent(t, srv) // join разобран, в очереди 2 места
	for range 3 {
		sendCommand(t, c, "noop", nil)
	}
	msg := readMessage(t, c)
	if msg.Type != MessageTypeError || !strings.Contains(string(msg.Data), "command queue full") {
		t.Fatalf("got %s %s, want queue full error", msg.Type, msg.Data)
	}
}

// Закрытие сессии сервером (например, при переполнении исходящей очереди)
// завершает чтение и даёт Leave.
func TestServerCloseGivesLeave(t *testing.T) {
	srv, url := startServer(t, 16)
	dial(t, url+"?player=p1")
	join := nextEvent(t, srv)
	join.Session.Close()
	leave := nextEvent(t, srv)
	if leave.Kind != EventLeave || leave.Session != join.Session {
		t.Fatalf("event %+v, want leave", leave)
	}
}

// Без допустимого id игрока соединение не открывается.
func TestServerRejectsMissingOrInvalidPlayer(t *testing.T) {
	_, url := startServer(t, 16)
	for _, q := range []string{"", "?player=", "?player=Alice", "?player=a%20b", "?player=" + strings.Repeat("x", 33)} {
		c, resp, err := websocket.DefaultDialer.Dial(url+q, nil)
		if err == nil {
			c.Close()
			t.Fatalf("query %q: connection accepted", q)
		}
		if resp == nil || resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("query %q: response %v, want 400", q, resp)
		}
	}
}

// Две сессии одного игрока получают его сообщение, собранное один раз;
// другой игрок получает своё.
func TestHubBroadcastPerPlayer(t *testing.T) {
	h := NewHub()
	ca1, ca2, cb := newFakeConn(false), newFakeConn(false), newFakeConn(false)
	a1, a2, b := newSession(1, "alice", ca1, 8), newSession(2, "alice", ca2, 8), newSession(3, "bob", cb, 8)
	for _, s := range []*Session{a1, a2, b} {
		h.Add(s)
		defer s.Close()
	}
	calls := map[string]int{}
	h.BroadcastPerPlayer(MessageTypeState, func(playerID string) interface{} {
		calls[playerID]++
		return map[string]string{"player": playerID}
	})
	if calls["alice"] != 1 || calls["bob"] != 1 || len(calls) != 2 {
		t.Fatalf("payload calls %v, want once per player", calls)
	}
	for conn, want := range map[*fakeConn]string{ca1: "alice", ca2: "alice", cb: "bob"} {
		w := waitWritten(t, conn, 1)
		var msg Message
		if err := json.Unmarshal(w[0], &msg); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !strings.Contains(string(msg.Data), `"player":"`+want+`"`) {
			t.Fatalf("session of %s got %s", want, msg.Data)
		}
	}
}
