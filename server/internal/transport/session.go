package transport

import (
	"log"
	"sync"

	"github.com/gorilla/websocket"
)

// OutboxSize - ёмкость исходящей очереди сессии. Временное значение:
// настоящие пределы (слот состояния на 1, около 64 уведомлений, таймаут
// записи) - задача 1.2 в TODO.md.
const OutboxSize = 32

// wsConn - то, что сессии нужно от соединения gorilla. Интерфейс позволяет
// проверять логику сессии в тестах без сети.
type wsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	Close() error
}

// outFrame - исходящее сообщение; last закрывает сессию после отправки.
type outFrame struct {
	data []byte
	last bool
}

// Session - сетевой край одного подключения (актор-сессия, см.
// documentation/architecture.md, «Конкурентная модель»). Читает сокет
// горутина чтения Server, пишет только горутина записи writeLoop: gorilla
// допускает одного писателя на соединение. Игрового состояния сессия не
// держит.
//
// Send безопасен из любой горутины и никогда не блокирует: медленный клиент
// не тормозит цикл симуляции, при переполнении очереди соединение рвётся.
type Session struct {
	// ID - номер сессии в пределах процесса, для логов и порядка обхода.
	ID uint64
	// PlayerID - игрок, от имени которого идут команды сессии. Задаётся
	// при подключении и дальше не меняется.
	PlayerID string

	conn      wsConn
	out       chan outFrame
	done      chan struct{}
	closeOnce sync.Once
}

// newSession создаёт сессию и запускает её горутину записи.
func newSession(id uint64, playerID string, conn wsConn, outboxSize int) *Session {
	s := &Session{
		ID:       id,
		PlayerID: playerID,
		conn:     conn,
		out:      make(chan outFrame, outboxSize),
		done:     make(chan struct{}),
	}
	go s.writeLoop()
	return s
}

// Send ставит сообщение в исходящую очередь без блокировки.
// Returns: false, если сессия закрыта или очередь переполнена (тогда сессия
// закрывается: клиент получит полное состояние при переподключении).
func (s *Session) Send(data []byte) bool {
	return s.enqueue(outFrame{data: data})
}

// SendAndClose отправляет последнее сообщение и закрывает сессию.
func (s *Session) SendAndClose(data []byte) {
	if !s.enqueue(outFrame{data: data, last: true}) {
		s.Close()
	}
}

func (s *Session) enqueue(f outFrame) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	select {
	case s.out <- f:
		return true
	default:
		log.Printf("session %d (player %q): outbox full, closing", s.ID, s.PlayerID)
		s.Close()
		return false
	}
}

// Close закрывает сессию и соединение; повторный вызов ничего не делает.
// Закрытие соединения прерывает горутину чтения.
func (s *Session) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		if err := s.conn.Close(); err != nil {
			log.Printf("session %d: close connection: %v", s.ID, err)
		}
	})
}

// Done закрывается вместе с сессией.
func (s *Session) Done() <-chan struct{} {
	return s.done
}

// writeLoop - единственный писатель в соединение.
func (s *Session) writeLoop() {
	for {
		select {
		case <-s.done:
			return
		case f := <-s.out:
			if err := s.conn.WriteMessage(websocket.TextMessage, f.data); err != nil {
				s.Close()
				return
			}
			if f.last {
				msg := websocket.FormatCloseMessage(websocket.ClosePolicyViolation, "")
				_ = s.conn.WriteMessage(websocket.CloseMessage, msg)
				s.Close()
				return
			}
		}
	}
}
