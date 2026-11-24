package transport

import (
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// nextCommand возвращает следующую команду из очереди, пропуская вход и
// выход сессий, и освобождает её место, как цикл симуляции.
func nextCommand(t *testing.T, srv *Server) Event {
	t.Helper()
	for {
		ev := nextEvent(t, srv)
		if ev.Kind == EventCommand {
			ev.Taken()
			return ev
		}
	}
}

func noCommand(t *testing.T, srv *Server) {
	t.Helper()
	deadline := time.After(80 * time.Millisecond)
	for {
		select {
		case ev := <-srv.Events():
			if ev.Kind == EventCommand {
				t.Fatalf("repeat reached the queue: %+v", ev.Command)
			}
		case <-deadline:
			return
		}
	}
}

func resend(t *testing.T, c *websocket.Conn, cmdID string) {
	t.Helper()
	sendRaw(t, c, Message{Type: MessageTypeCommand, Cmd: "noop", CmdID: cmdID})
}

func readAck(t *testing.T, c *websocket.Conn, cmdID string) {
	t.Helper()
	if msg := readMessage(t, c); msg.Type != MessageTypeAck || msg.CmdID != cmdID {
		t.Fatalf("got %s cmd_id %q %s, want ack for %q", msg.Type, msg.CmdID, msg.Data, cmdID)
	}
}

// noMessage проверяет, что клиенту больше ничего не пришло.
func noMessage(t *testing.T, c *websocket.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(80 * time.Millisecond))
	var msg Message
	if err := c.ReadJSON(&msg); err == nil {
		t.Fatalf("unexpected message %s cmd_id %q %s", msg.Type, msg.CmdID, msg.Data)
	}
}

func startDedupServer(t *testing.T, edit func(*Limits)) (*Server, string) {
	t.Helper()
	srv, url, _ := startLimitedServer(t, func(l *Limits) {
		l.CommandBurst = 1000 // лимит частоты здесь не проверяется
		edit(l)
	})
	return srv, url
}

// Повтор выполненной команды получает тот же ack и в очередь не попадает.
func TestRepeatAfterAck(t *testing.T) {
	srv, url := startDedupServer(t, func(*Limits) {})
	c := dial(t, url+"?player=alice")
	id := sendCommand(t, c, "noop", nil)
	nextCommand(t, srv).Reply(EncodeReply(MessageTypeAck, id, nil), true)
	readAck(t, c, id)

	resend(t, c, id)
	readAck(t, c, id)
	noCommand(t, srv)
}

// Повтор команды, завершившейся ошибкой, получает ту же ошибку.
func TestRepeatAfterError(t *testing.T) {
	srv, url := startDedupServer(t, func(*Limits) {})
	c := dial(t, url+"?player=alice")
	id := sendCommand(t, c, "noop", nil)
	nextCommand(t, srv).Reply(EncodeCommandError(id, CodeCommandFailed, "not enough wood"), true)
	readError(t, c, CodeCommandFailed, id)

	resend(t, c, id)
	readError(t, c, CodeCommandFailed, id)
	noCommand(t, srv)
}

// Повтор, пока команда ещё выполняется, не ставит её второй раз; ответ
// приходит один раз.
func TestRepeatWhilePending(t *testing.T) {
	srv, url := startDedupServer(t, func(*Limits) {})
	c := dial(t, url+"?player=alice")
	id := sendCommand(t, c, "noop", nil)
	ev := nextCommand(t, srv)
	resend(t, c, id)
	resend(t, c, id)
	noCommand(t, srv)

	ev.Reply(EncodeReply(MessageTypeAck, id, nil), true)
	readAck(t, c, id)
	noMessage(t, c)
}

// Повтор после переподключения: память у игрока, а не у сессии. Ответ на
// команду, шедшую во время обрыва, получает новая сессия.
func TestRepeatAfterReconnect(t *testing.T) {
	srv, url := startDedupServer(t, func(*Limits) {})
	old := dial(t, url+"?player=alice")
	done := sendCommand(t, old, "noop", nil)
	nextCommand(t, srv).Reply(EncodeReply(MessageTypeAck, done, nil), true)
	readAck(t, old, done)
	pending := sendCommand(t, old, "noop", nil)
	ev := nextCommand(t, srv)
	old.Close()

	c := dial(t, url+"?player=alice")
	resend(t, c, done)
	readAck(t, c, done)
	resend(t, c, pending)
	noCommand(t, srv)
	ev.Reply(EncodeReply(MessageTypeAck, pending, nil), true)
	readAck(t, c, pending)

	// Другой игрок с тем же cmd_id - другая команда.
	b := dial(t, url+"?player=bob")
	resend(t, b, done)
	if got := nextCommand(t, srv); got.Session.PlayerID != "bob" || got.Command.ID != done {
		t.Fatalf("command %+v, want bob's %q", got.Command, done)
	}
}

// Ответ команды, которая только читает мир, не запоминается: повтор
// выполняется заново.
func TestReadOnlyReplyNotRemembered(t *testing.T) {
	srv, url := startDedupServer(t, func(*Limits) {})
	c := dial(t, url+"?player=alice")
	id := sendCommand(t, c, "get_map", nil)
	nextCommand(t, srv).Reply(EncodeReply(MessageTypeMap, id, []int{1}), false)
	readMessage(t, c)
	resend(t, c, id)
	if ev := nextCommand(t, srv); ev.Command.ID != id {
		t.Fatalf("command %+v, want repeat of %q", ev.Command, id)
	}
}

// Отказ server_busy не запоминается: повтор с тем же cmd_id выполняется.
func TestBusyRejectNotRemembered(t *testing.T) {
	srv, url := startDedupServer(t, func(l *Limits) { l.MaxQueuedPerPlayer = 1 })
	c := dial(t, url+"?player=alice")
	first := sendCommand(t, c, "noop", nil)
	ev := nextEvent(t, srv)
	for ev.Kind != EventCommand {
		ev = nextEvent(t, srv)
	}
	busy := sendCommand(t, c, "noop", nil) // место игрока занято: Taken не вызван
	readError(t, c, CodeServerBusy, busy)
	ev.Taken()
	ev.Reply(EncodeReply(MessageTypeAck, first, nil), true)
	readAck(t, c, first)

	resend(t, c, busy)
	if got := nextCommand(t, srv); got.Command.ID != busy {
		t.Fatalf("command %+v, want repeat of %q", got.Command, busy)
	}
}

// Память ограничена: вытесненный cmd_id выполняется заново, свежий - нет.
func TestReplyMemoryEviction(t *testing.T) {
	srv, url := startDedupServer(t, func(l *Limits) { l.ReplyMemory = 2 })
	c := dial(t, url+"?player=alice")
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = sendCommand(t, c, "noop", nil)
		nextCommand(t, srv).Reply(EncodeReply(MessageTypeAck, ids[i], nil), true)
		readAck(t, c, ids[i])
	}
	resend(t, c, ids[2])
	readAck(t, c, ids[2])
	noCommand(t, srv)
	resend(t, c, ids[0])
	if got := nextCommand(t, srv); got.Command.ID != ids[0] {
		t.Fatalf("command %+v, want evicted %q executed again", got.Command, ids[0])
	}
}

// forget и снятие непомнимого ответа не оставляют в порядке вытеснения
// чужих записей: размер порядка равен числу записей.
func TestReplyMemoryForgetKeepsOrderConsistent(t *testing.T) {
	m := newReplyMemory(3)
	s := &Session{}
	for _, id := range []string{"a", "b", "a", "c", "d", "e"} {
		if st, _ := m.begin(id, s); st == replyNew && id == "a" {
			m.forget("a", s)
		}
	}
	m.complete("d", []byte("x"), false)
	if len(m.order) != len(m.byID) {
		t.Fatalf("order %v, entries %d", m.order, len(m.byID))
	}
	if _, ok := m.byID["c"]; !ok {
		t.Fatalf("c evicted early, order %v", m.order)
	}
}
