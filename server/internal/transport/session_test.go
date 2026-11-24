package transport

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	deadline   atomic.Int64 // UnixNano, 0 - без таймаута
	readDL     atomic.Int64 // UnixNano последнего SetReadDeadline
	pong       atomic.Pointer[func(string) error]
}

func (c *fakeConn) SetWriteDeadline(t time.Time) error {
	c.deadline.Store(t.UnixNano())
	return nil
}

func (c *fakeConn) SetReadDeadline(t time.Time) error {
	c.readDL.Store(t.UnixNano())
	return nil
}

func (c *fakeConn) SetPongHandler(h func(string) error) {
	c.pong.Store(&h)
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
		var timeout <-chan time.Time
		if d := c.deadline.Load(); d != 0 {
			timeout = time.After(time.Until(time.Unix(0, d)))
		}
		select {
		case <-c.gate:
		case <-c.closed:
			return errors.New("closed")
		case <-timeout:
			return errors.New("write timeout")
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

// Слот состояния: новое состояние затирает неотправленное, переполнения и
// разрыва нет; клиент получает последнее.
func TestStateSlotOverwrites(t *testing.T) {
	conn := newFakeConn(true)
	s := newSession(1, "p1", conn, 4)
	defer s.Close()
	for i := range 100 {
		if !s.SendState([]byte{byte(i)}) {
			t.Fatalf("SendState %d returned false", i)
		}
	}
	close(conn.gate)
	time.Sleep(50 * time.Millisecond)
	written, _ := conn.snapshot()
	// Первое состояние писатель мог забрать до затирания; дальше - только последнее.
	if len(written) == 0 || len(written) > 2 || written[len(written)-1][0] != 99 {
		t.Fatalf("written %v, want at most 2 states ending with 99", written)
	}
	if s.isClosed() {
		t.Fatal("state overflow closed the session")
	}
}

// Уведомления пишутся раньше ожидающего состояния.
func TestNotificationsBeforeState(t *testing.T) {
	conn := newFakeConn(true)
	s := newSession(1, "p1", conn, 4)
	defer s.Close()
	s.Send([]byte("n0"))
	waitWriters(t, conn) // писатель держит n0 в WriteMessage
	s.SendState([]byte("s"))
	s.Send([]byte("n1"))
	s.Send([]byte("n2"))
	close(conn.gate)
	written := waitWritten(t, conn, 4)
	if got := string(bytesJoin(written)); got != "n0n1n2s" {
		t.Fatalf("order %q, want n0n1n2s", got)
	}
}

// Клиент, который не читает дольше таймаута записи, отключается.
func TestWriteTimeoutCloses(t *testing.T) {
	conn := newFakeConn(true)
	s := newSessionTimeout(1, "p1", conn, 4, 50*time.Millisecond)
	start := time.Now()
	s.SendState([]byte("state"))
	waitClosed(t, s)
	if d := time.Since(start); d < 40*time.Millisecond {
		t.Fatalf("closed after %v, before the write timeout", d)
	}
}

// Сервер шлёт ping через писателя сессии с заданным интервалом, не
// нарушая правило одного писателя.
func TestPingSentPeriodically(t *testing.T) {
	conn := newFakeConn(false)
	s := newSessionKeepalive(1, "p1", conn, 4, WriteTimeout, 10*time.Millisecond, 0)
	defer s.Close()
	s.Send([]byte("n"))
	deadline := time.Now().Add(testWait)
	for {
		_, types := conn.snapshot()
		pings := 0
		for _, ty := range types {
			if ty == websocket.PingMessage {
				pings++
			}
		}
		if pings >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("got %d pings, want at least 3", pings)
		}
		time.Sleep(time.Millisecond)
	}
	if m := conn.maxWriters.Load(); m != 1 {
		t.Fatalf("max concurrent writers %d, want 1", m)
	}
}

// Без ping (интервал 0) ничего лишнего в сокет не пишется.
func TestNoPingWhenDisabled(t *testing.T) {
	conn := newFakeConn(false)
	s := newSessionTimeout(1, "p1", conn, 4, WriteTimeout)
	defer s.Close()
	time.Sleep(30 * time.Millisecond)
	if w, _ := conn.snapshot(); len(w) != 0 {
		t.Fatalf("wrote %d messages, want none", len(w))
	}
}

// Read deadline ставится при создании сессии, pong и принятое сообщение
// отодвигают его на ReadTimeout.
func TestReadDeadlineArmedAndExtended(t *testing.T) {
	conn := newFakeConn(false)
	const timeout = time.Minute
	before := time.Now()
	s := newSessionKeepalive(1, "p1", conn, 4, WriteTimeout, 0, timeout)
	defer s.Close()
	first := time.Unix(0, conn.readDL.Load())
	if first.Before(before.Add(timeout)) || first.After(time.Now().Add(timeout)) {
		t.Fatalf("initial read deadline %v, want about now+%v", first, timeout)
	}
	h := conn.pong.Load()
	if h == nil {
		t.Fatal("pong handler is not set")
	}
	time.Sleep(5 * time.Millisecond)
	if err := (*h)(""); err != nil {
		t.Fatalf("pong handler: %v", err)
	}
	afterPong := time.Unix(0, conn.readDL.Load())
	if !afterPong.After(first) {
		t.Fatalf("pong did not extend the read deadline: %v -> %v", first, afterPong)
	}
	time.Sleep(5 * time.Millisecond)
	s.extendRead()
	if !time.Unix(0, conn.readDL.Load()).After(afterPong) {
		t.Fatal("received message did not extend the read deadline")
	}
}

// Без таймаута чтения (ReadTimeout 0) deadline и обработчик pong не ставятся.
func TestNoReadDeadlineWhenDisabled(t *testing.T) {
	conn := newFakeConn(false)
	s := newSessionTimeout(1, "p1", conn, 4, WriteTimeout)
	defer s.Close()
	if conn.readDL.Load() != 0 || conn.pong.Load() != nil {
		t.Fatal("read deadline or pong handler set with the timeout disabled")
	}
}

func waitWriters(t *testing.T, c *fakeConn) {
	t.Helper()
	deadline := time.Now().Add(testWait)
	for c.writers.Load() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("writer did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

func bytesJoin(parts [][]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
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

// Hub держит сессии по возрастанию ID, снятая сессия больше ничего не получает.
func TestHubSessionsAndRemove(t *testing.T) {
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
	h.Remove(s2)
	h.BroadcastPerPlayer(MessageTypeState, func(string) interface{} { return 1 })
	waitWritten(t, c1, 1)
	time.Sleep(20 * time.Millisecond)
	if w, _ := c2.snapshot(); len(w) != 0 {
		t.Fatalf("removed session got %d messages, want 0", len(w))
	}
}

// Ответ на команду кодируется с её cmd_id, ошибка - с кодом.
func TestEncodeReplyCarriesCmdID(t *testing.T) {
	var ack Message
	if err := json.Unmarshal(EncodeReply(MessageTypeAck, "c1", nil), &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Type != MessageTypeAck || ack.CmdID != "c1" || ack.Data != nil {
		t.Fatalf("ack %+v", ack)
	}
	var msg Message
	if err := json.Unmarshal(EncodeCommandError("c2", CodeCommandFailed, "boom"), &msg); err != nil {
		t.Fatal(err)
	}
	var data ErrorData
	if err := json.Unmarshal(msg.Data, &data); err != nil {
		t.Fatal(err)
	}
	if msg.Type != MessageTypeError || msg.CmdID != "c2" || data.Code != CodeCommandFailed || data.Error != "boom" {
		t.Fatalf("error %+v %+v", msg, data)
	}
}

// --- Сквозные проверки через настоящий WebSocket ---

func startServer(t *testing.T, queueSize int) (*Server, string) {
	t.Helper()
	l := DefaultLimits()
	l.QueueSize, l.OutboxSize = queueSize, 8
	srv := newServer(l)
	ts := httptest.NewServer(http.HandlerFunc(srv.HandleWebSocket))
	t.Cleanup(ts.Close)
	return srv, "ws" + strings.TrimPrefix(ts.URL, "http")
}

// startKeepaliveServer - сервер с короткими ping и таймаутом чтения.
func startKeepaliveServer(t *testing.T, ping, readTimeout time.Duration) (*Server, string) {
	t.Helper()
	l := DefaultLimits()
	l.QueueSize, l.OutboxSize = 16, 8
	l.PingInterval, l.ReadTimeout = ping, readTimeout
	srv := newServer(l)
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

var testCmdSeq atomic.Int64

// sendCommand отправляет команду с новым cmd_id и возвращает этот id.
func sendCommand(t *testing.T, c *websocket.Conn, name string, data any) string {
	t.Helper()
	id := "c" + strconv.FormatInt(testCmdSeq.Add(1), 10)
	sendRaw(t, c, Message{Type: MessageTypeCommand, Cmd: name, CmdID: id, Data: mustMarshal(data)})
	return id
}

func sendRaw(t *testing.T, c *websocket.Conn, msg Message) {
	t.Helper()
	if err := c.WriteJSON(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// readError читает сообщение и проверяет, что это ошибка с кодом code и cmd_id.
func readError(t *testing.T, c *websocket.Conn, code, cmdID string) {
	t.Helper()
	msg := readMessage(t, c)
	var data ErrorData
	_ = json.Unmarshal(msg.Data, &data)
	if msg.Type != MessageTypeError || data.Code != code || msg.CmdID != cmdID {
		t.Fatalf("got %s cmd_id %q %s, want error %s for %q", msg.Type, msg.CmdID, msg.Data, code, cmdID)
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
	id := sendCommand(t, c, "upgrade_building", map[string]string{"building": "main"})
	cmd := nextEvent(t, srv)
	if cmd.Kind != EventCommand || cmd.Session != join.Session || cmd.Command.ID != id || cmd.Command.Name != "upgrade_building" || cmd.Command.Data["building"] != "main" {
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
	var last string
	for range 3 {
		last = sendCommand(t, c, "noop", nil)
	}
	readError(t, c, CodeServerBusy, last)
}

// Команда без cmd_id или с неразборными data отклоняется bad_request и в
// очередь не попадает.
func TestServerRejectsBadCommand(t *testing.T) {
	srv, url := startServer(t, 16)
	c := dial(t, url+"?player=p1")
	nextEvent(t, srv)
	sendRaw(t, c, Message{Type: MessageTypeCommand, Cmd: "noop"})
	readError(t, c, CodeBadRequest, "")
	sendRaw(t, c, Message{Type: MessageTypeCommand, Cmd: "noop", CmdID: strings.Repeat("x", MaxCmdIDLen+1)})
	readError(t, c, CodeBadRequest, "")
	sendRaw(t, c, Message{Type: MessageTypeCommand, Cmd: "noop", CmdID: "c-bad", Data: json.RawMessage(`[1]`)})
	readError(t, c, CodeBadRequest, "c-bad")
	select {
	case ev := <-srv.Events():
		t.Fatalf("rejected command reached the queue: %+v", ev)
	default:
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

// Клиент, который не отвечает на ping (не читает сокет, pong не уходит),
// отключается по таймауту чтения и даёт Leave.
func TestServerClosesSilentClient(t *testing.T) {
	srv, url := startKeepaliveServer(t, 20*time.Millisecond, 150*time.Millisecond)
	dial(t, url+"?player=silent") // соединение открыто, но клиент ничего не читает
	join := nextEvent(t, srv)
	start := time.Now()
	leave := nextEvent(t, srv)
	if leave.Kind != EventLeave || leave.Session != join.Session {
		t.Fatalf("event %+v, want leave of the silent session", leave)
	}
	if d := time.Since(start); d < 100*time.Millisecond {
		t.Fatalf("closed after %v, before the read timeout", d)
	}
}

// Живой клиент (читает сокет, библиотека сама отвечает pong) не отключается,
// хотя молчит дольше таймаута чтения.
func TestServerKeepsClientAnsweringPings(t *testing.T) {
	srv, url := startKeepaliveServer(t, 20*time.Millisecond, 150*time.Millisecond)
	c := dial(t, url+"?player=alive")
	join := nextEvent(t, srv)
	go func() {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}()
	select {
	case ev := <-srv.Events():
		t.Fatalf("event %+v, a client answering pings was disconnected", ev)
	case <-time.After(500 * time.Millisecond):
	}
	if join.Session.isClosed() {
		t.Fatal("session of a live client was closed")
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
