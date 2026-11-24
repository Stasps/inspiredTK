package transport

import (
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Исходящие пределы сессии (documentation/architecture.md, «Очереди и
// переполнение»). Значения стартовые, уточняются нагрузочным тестом.
const (
	// OutboxSize - ёмкость очереди уведомлений сессии (hello, ответы на
	// команды, карта). При переполнении соединение рвётся: клиент получит
	// полное состояние при переподключении.
	OutboxSize = 64
	// WriteTimeout - предельное время записи одного сообщения в сокет;
	// клиент, который не читает дольше, отключается.
	WriteTimeout = 10 * time.Second
	// PingInterval - как часто сервер шлёт ping. Браузер отвечает pong сам.
	PingInterval = 30 * time.Second
	// ReadTimeout - сколько сессия может молчать (ни сообщений, ни pong),
	// прежде чем её закроют. Больше двух интервалов ping: один потерянный
	// pong не рвёт соединение, а полуоткрытое (клиент пропал без закрытия)
	// обнаруживается за минуту с небольшим.
	ReadTimeout = 70 * time.Second
)

// wsConn - то, что сессии нужно от соединения gorilla. Интерфейс позволяет
// проверять логику сессии в тестах без сети.
type wsConn interface {
	ReadMessage() (messageType int, p []byte, err error)
	WriteMessage(messageType int, data []byte) error
	SetWriteDeadline(t time.Time) error
	SetReadDeadline(t time.Time) error
	SetPongHandler(h func(appData string) error)
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
// Исходящих каналов два: очередь уведомлений (Send) и слот состояния на одно
// сообщение (SendState), где новое состояние затирает неотправленное старое.
// Уведомления пишутся раньше состояния. Send и SendState безопасны из любой
// горутины и никогда не блокируют: медленный клиент не тормозит цикл
// симуляции; переполнение уведомлений или таймаут записи рвут соединение.
type Session struct {
	// ID - номер сессии в пределах процесса, для логов и порядка обхода.
	ID uint64
	// PlayerID - игрок, от имени которого идут команды сессии. Задаётся
	// при подключении и дальше не меняется.
	PlayerID string

	// limiter - входящие пределы игрока, общие для его сессий; задаётся
	// Server до первой команды.
	limiter *playerLimiter

	conn         wsConn
	writeTimeout time.Duration
	pingEvery    time.Duration // 0 - ping выключен
	readTimeout  time.Duration // 0 - read deadline не ставится
	out          chan outFrame

	stateMu    sync.Mutex
	state      []byte
	stateReady chan struct{}

	done      chan struct{}
	closeOnce sync.Once
}

// newSession создаёт сессию с таймаутом записи WriteTimeout.
func newSession(id uint64, playerID string, conn wsConn, outboxSize int) *Session {
	return newSessionTimeout(id, playerID, conn, outboxSize, WriteTimeout)
}

// newSessionTimeout создаёт сессию без ping и без read deadline.
func newSessionTimeout(id uint64, playerID string, conn wsConn, outboxSize int, writeTimeout time.Duration) *Session {
	return newSessionKeepalive(id, playerID, conn, outboxSize, writeTimeout, 0, 0)
}

// newSessionKeepalive создаёт сессию и запускает её горутину записи. Если
// pingEvery > 0, писатель шлёт ping с этим интервалом; если readTimeout > 0,
// чтение ждёт сообщение или pong не дольше readTimeout, иначе
// ReadMessage вернёт ошибку и сессия закроется.
func newSessionKeepalive(id uint64, playerID string, conn wsConn, outboxSize int, writeTimeout, pingEvery, readTimeout time.Duration) *Session {
	s := &Session{
		ID:           id,
		PlayerID:     playerID,
		conn:         conn,
		writeTimeout: writeTimeout,
		pingEvery:    pingEvery,
		readTimeout:  readTimeout,
		out:          make(chan outFrame, outboxSize),
		stateReady:   make(chan struct{}, 1),
		done:         make(chan struct{}),
	}
	if readTimeout > 0 {
		conn.SetPongHandler(func(string) error {
			s.extendRead()
			return nil
		})
		s.extendRead()
	}
	go s.writeLoop()
	return s
}

// extendRead отодвигает read deadline на readTimeout от сейчас. Зовётся при
// создании, на каждый pong и на каждое принятое сообщение.
func (s *Session) extendRead() {
	if s.readTimeout > 0 {
		_ = s.conn.SetReadDeadline(time.Now().Add(s.readTimeout))
	}
}

// Send ставит уведомление в исходящую очередь без блокировки.
// Returns: false, если сессия закрыта или очередь переполнена (тогда сессия
// закрывается: клиент получит полное состояние при переподключении).
func (s *Session) Send(data []byte) bool {
	return s.enqueue(outFrame{data: data})
}

// SendState кладёт состояние в слот сессии, затирая неотправленное
// предыдущее: клиенту нужно только последнее. Не блокирует.
// Returns: false, если сессия закрыта.
func (s *Session) SendState(data []byte) bool {
	select {
	case <-s.done:
		return false
	default:
	}
	s.stateMu.Lock()
	s.state = data
	s.stateMu.Unlock()
	select {
	case s.stateReady <- struct{}{}:
	default: // сигнал уже взведён, писатель заберёт свежее состояние
	}
	return true
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

// writeLoop - единственный писатель в соединение. Уведомления идут раньше
// состояния: hello приходит до первого state, ответы не ждут за ним.
func (s *Session) writeLoop() {
	// ping идёт из того же писателя: gorilla допускает одного писателя.
	var pingC <-chan time.Time
	if s.pingEvery > 0 {
		t := time.NewTicker(s.pingEvery)
		defer t.Stop()
		pingC = t.C
	}
	for {
		select {
		case f := <-s.out:
			if !s.writeFrame(f) {
				return
			}
			continue
		default:
		}
		select {
		case <-s.done:
			return
		case f := <-s.out:
			if !s.writeFrame(f) {
				return
			}
		case <-pingC:
			if !s.write(websocket.PingMessage, nil) {
				return
			}
		case <-s.stateReady:
			s.stateMu.Lock()
			data := s.state
			s.state = nil
			s.stateMu.Unlock()
			if data != nil && !s.write(websocket.TextMessage, data) {
				return
			}
		}
	}
}

// writeFrame пишет уведомление. Returns: false, если писатель завершается.
func (s *Session) writeFrame(f outFrame) bool {
	if !s.write(websocket.TextMessage, f.data) {
		return false
	}
	if f.last {
		s.write(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, ""))
		s.Close()
		return false
	}
	return true
}

// write пишет одно сообщение с таймаутом; при ошибке закрывает сессию.
func (s *Session) write(messageType int, data []byte) bool {
	if err := s.conn.SetWriteDeadline(time.Now().Add(s.writeTimeout)); err != nil {
		s.Close()
		return false
	}
	if err := s.conn.WriteMessage(messageType, data); err != nil {
		select {
		case <-s.done: // закрыта нами, не ошибка клиента
		default:
			log.Printf("session %d (player %q): write: %v, closing", s.ID, s.PlayerID, err)
		}
		s.Close()
		return false
	}
	return true
}
