package transport

import "sync"

// Дедупликация повторов по cmd_id (documentation/architecture.md, «Очереди и
// переполнение»: повтор безопасен благодаря cmd_id). Сервер помнит последние
// cmd_id игрока, дошедшие до симуляции, и на повтор шлёт сохранённый ответ,
// не выполняя команду снова. Память - у игрока, а не у сессии: повтор после
// переподключения тоже узнаётся. Отказы до очереди (rate_limited,
// server_busy, bad_request) не запоминаются: команда не выполнялась, и повтор
// с тем же cmd_id должен её выполнить.

// replyEntry - команда игрока: ждёт выполнения (reply == nil) или выполнена.
type replyEntry struct {
	reply []byte
	// waiters - сессии, ждущие ответа: отправитель и сессии, приславшие
	// повтор, пока команда шла (например, после переподключения).
	waiters []*Session
}

// replyMemory - последние limit cmd_id игрока с ответами; самый старый
// вытесняется первым.
type replyMemory struct {
	mu    sync.Mutex
	limit int
	byID  map[string]*replyEntry
	order []string // cmd_id по порядку поступления, order[0] - самый старый
}

func newReplyMemory(limit int) *replyMemory {
	return &replyMemory{limit: limit, byID: make(map[string]*replyEntry, limit)}
}

// replyState - что делать с пришедшей командой.
type replyState int

const (
	replyNew     replyState = iota // новый cmd_id: выполнить
	replyPending                   // уже идёт: ответ придёт и этой сессии
	replyDone                      // выполнена: отправить сохранённый ответ
)

// begin регистрирует команду cmdID от сессии s.
// Returns: replyNew - команда записана как ожидающая, её нужно поставить в
// очередь (или снять forget при отказе); replyPending - сессия добавлена к
// ожидающим; replyDone и сохранённый ответ.
func (m *replyMemory) begin(cmdID string, s *Session) (replyState, []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.byID[cmdID]; ok {
		if e.reply != nil {
			return replyDone, e.reply
		}
		for _, w := range e.waiters {
			if w == s {
				return replyPending, nil
			}
		}
		e.waiters = append(e.waiters, s)
		return replyPending, nil
	}
	if len(m.order) >= m.limit {
		// Вытесняется самый старый. Ожидающих у игрока не больше
		// MaxQueuedPerPlayer, поэтому при limit много больше лимита очереди
		// это уже выполненная команда (иначе ответ всё равно уйдёт
		// отправителю, см. Event.Reply).
		delete(m.byID, m.order[0])
		m.order = append(m.order[:0], m.order[1:]...)
	}
	m.order = append(m.order, cmdID)
	m.byID[cmdID] = &replyEntry{waiters: []*Session{s}}
	return replyNew, nil
}

// forget снимает ожидающую команду, не дошедшую до симуляции (отказ при
// постановке в очередь), чтобы повтор выполнил её. Сессии, присоединившиеся
// повтором, получат тот же отказ.
// Returns: ожидающие сессии, кроме отправителя.
func (m *replyMemory) forget(cmdID string, sender *Session) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byID[cmdID]
	if !ok || e.reply != nil {
		return nil
	}
	m.removeLocked(cmdID)
	return without(e.waiters, sender)
}

// complete сохраняет ответ команды (remember) или снимает её из памяти и
// возвращает сессии, которым его нужно отправить.
func (m *replyMemory) complete(cmdID string, reply []byte, remember bool) []*Session {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byID[cmdID]
	if !ok || e.reply != nil {
		return nil
	}
	waiters := e.waiters
	e.waiters = nil
	if remember {
		e.reply = reply
	} else {
		m.removeLocked(cmdID)
	}
	return waiters
}

func (m *replyMemory) removeLocked(cmdID string) {
	delete(m.byID, cmdID)
	for i, id := range m.order {
		if id == cmdID {
			m.order = append(m.order[:i], m.order[i+1:]...)
			return
		}
	}
}

func without(list []*Session, s *Session) []*Session {
	out := make([]*Session, 0, len(list))
	for _, w := range list {
		if w != s {
			out = append(out, w)
		}
	}
	return out
}
