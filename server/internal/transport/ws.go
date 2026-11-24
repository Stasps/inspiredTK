// Package transport provides WebSocket communication between server and clients.
package transport

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// MessageType defines the type of WebSocket message.
type MessageType string

// Message types for client-server communication.
const (
	MessageTypeState   MessageType = "state"
	MessageTypeCommand MessageType = "command"
	MessageTypeError   MessageType = "error"
	MessageTypeMap     MessageType = "map"
	// MessageTypeHello - первое сообщение сессии: playerId и поселения игрока.
	MessageTypeHello MessageType = "hello"
	// MessageTypeAck - успешное выполнение команды с cmd_id.
	MessageTypeAck MessageType = "ack"
)

// MaxCmdIDLen - предельная длина cmd_id команды.
const MaxCmdIDLen = 64

// MaxMessageSize - предельный размер входящего сообщения клиента в байтах.
// Команды малы; большее сообщение рвёт соединение (защита памяти сервера).
const MaxMessageSize = 16 << 10

// Коды ошибок в data.code сообщения error.
const (
	// CodeBadRequest - сообщение или команда не разобраны (нет cmd_id, неверные data).
	CodeBadRequest = "bad_request"
	// CodeCommandFailed - симуляция отклонила команду.
	CodeCommandFailed = "command_failed"
	// CodeRateLimited - превышен лимит частоты команд игрока.
	CodeRateLimited = "rate_limited"
	// CodeServerBusy - очередь команд симуляции заполнена; повтор безопасен.
	CodeServerBusy = "server_busy"
	// CodeJoinFailed - игрока не удалось впустить в мир.
	CodeJoinFailed = "join_failed"
)

// playerIDPattern - допустимый id игрока в параметре подключения.
var playerIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// ValidPlayerID сообщает, допустим ли id игрока.
func ValidPlayerID(id string) bool {
	return playerIDPattern.MatchString(id)
}

// Message represents a WebSocket message. CmdID - идентификатор команды,
// который задаёт клиент; ответ на команду (ack, error, map) несёт тот же CmdID.
type Message struct {
	Type  MessageType     `json:"type"`
	Cmd   string          `json:"cmd,omitempty"`
	CmdID string          `json:"cmd_id,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// ErrorData - содержимое сообщения error.
type ErrorData struct {
	Code  string `json:"code"`
	Error string `json:"error"`
}

// Command represents a client command queued for execution by the world actor.
type Command struct {
	// ID - cmd_id клиента: ответ уходит только отправителю с этим id.
	ID   string
	Name string
	Data map[string]interface{}
}

// EventKind - вид события сетевого края для цикла симуляции.
type EventKind int

// Виды событий сетевого края.
const (
	EventJoin    EventKind = iota + 1 // сессия подключилась
	EventLeave                        // сессия отключилась, больше событий от неё не будет
	EventCommand                      // команда клиента
)

// Event - событие сетевого края. Для одной сессии порядок гарантирован:
// EventJoin, затем её команды, затем EventLeave.
type Event struct {
	Kind    EventKind
	Session *Session
	Command Command // только для EventCommand
}

// Taken сообщает транспорту, что симуляция забрала команду из очереди, и
// освобождает место игрока в лимите MaxQueuedPerPlayer. Цикл симуляции
// вызывает его для каждого полученного EventCommand; для других видов
// событий ничего не делает.
func (ev Event) Taken() {
	if ev.Kind == EventCommand && ev.Session != nil && ev.Session.limiter != nil {
		ev.Session.limiter.release()
	}
}

// Reply отправляет ответ на команду (ack, ошибку или данные с её cmd_id)
// только отправителю и сессиям игрока, приславшим тот же cmd_id, пока
// команда шла. remember сохраняет ответ для будущих повторов; для команд,
// которые только читают мир (get_map), повтор просто выполняется заново.
// Вызывается из цикла симуляции один раз на EventCommand.
func (ev Event) Reply(data []byte, remember bool) {
	var waiters []*Session
	if ev.Session.limiter != nil {
		waiters = ev.Session.limiter.replies.complete(ev.Command.ID, data, remember)
	}
	if len(waiters) == 0 {
		// cmd_id уже вытеснен из памяти (или сессия без игрока в тестах):
		// ответ всё равно получает отправитель.
		waiters = []*Session{ev.Session}
	}
	for _, s := range waiters {
		s.Send(data)
	}
}

// Server принимает WebSocket-соединения: на каждое - сессия с горутиной
// чтения (HandleWebSocket) и горутиной записи (Session.writeLoop).
// События сессий читает единственная горутина симуляции через Events.
type Server struct {
	upgrader websocket.Upgrader
	events   chan Event
	lastID   atomic.Uint64
	limits   Limits
	limiters *limiterRegistry
	now      func() time.Time
}

// NewServer creates a new WebSocket server with DefaultLimits.
func NewServer() *Server {
	return newServer(DefaultLimits())
}

func newServer(limits Limits) *Server {
	return &Server{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// Allow all origins for development
				return true
			},
		},
		events:   make(chan Event, limits.QueueSize),
		limits:   limits,
		limiters: newLimiterRegistry(limits.CommandBurst, limits.ReplyMemory),
		now:      time.Now,
	}
}

// Events возвращает канал событий сессий для цикла симуляции.
func (s *Server) Events() <-chan Event {
	return s.events
}

// HandleWebSocket обслуживает подключение; это горутина чтения сессии.
// Игрок задаётся параметром ?player=<id> (без авторизации, только для
// разработки); все команды сессии идут от его имени.
func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	playerID := r.URL.Query().Get("player")
	if !ValidPlayerID(playerID) {
		http.Error(w, "query parameter player must match [a-z0-9_-]{1,32}", http.StatusBadRequest)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	conn.SetReadLimit(MaxMessageSize)
	sess := newSessionKeepalive(s.lastID.Add(1), playerID, conn, s.limits.OutboxSize, s.limits.WriteTimeout, s.limits.PingInterval, s.limits.ReadTimeout)
	log.Printf("session %d connected: player %q from %s", sess.ID, playerID, conn.RemoteAddr())
	s.serve(sess)
	log.Printf("session %d disconnected", sess.ID)
}

// serve ведёт сессию от подключения до отключения. Join и Leave ставятся в
// очередь с ожиданием: блокируется только горутина этой сессии, а не
// симуляция, и цикл симуляции не теряет ни одной сессии.
func (s *Server) serve(sess *Session) {
	sess.limiter = s.limiters.acquire(sess.PlayerID, s.now())
	s.events <- Event{Kind: EventJoin, Session: sess}
	s.readLoop(sess)
	sess.Close()
	s.events <- Event{Kind: EventLeave, Session: sess}
}

// readLoop читает сообщения до ошибки чтения или закрытия сессии.
// Лимит частоты применяется к каждому входящему сообщению, включая
// неразборные: спам мусором ограничен так же, как спам командами.
func (s *Server) readLoop(sess *Session) {
	strikes := 0
	for {
		_, data, err := sess.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
				log.Printf("session %d: read: %v", sess.ID, err)
			}
			return
		}
		sess.extendRead()
		var msg Message
		parseErr := json.Unmarshal(data, &msg)
		if !sess.limiter.allow(s.now(), s.limits.CommandRate, s.limits.CommandBurst) {
			strikes++
			if strikes >= s.limits.MaxRateStrikes {
				log.Printf("session %d (player %q): %d rate-limited messages in a row, closing", sess.ID, sess.PlayerID, strikes)
				sess.SendAndClose(EncodeCommandError(msg.CmdID, CodeRateLimited, "too many commands, disconnecting"))
				// Писатель сам закроет сессию после отказа и кадра закрытия;
				// ранний выход закрыл бы соединение до их отправки.
				<-sess.Done()
				return
			}
			sess.Send(EncodeCommandError(msg.CmdID, CodeRateLimited, "too many commands, retry later"))
			continue
		}
		strikes = 0
		if parseErr != nil {
			sess.Send(EncodeError(CodeBadRequest, "invalid message: "+parseErr.Error()))
			continue
		}
		if msg.Type == MessageTypeCommand {
			s.enqueueCommand(sess, msg)
		}
	}
}

// enqueueCommand ставит команду в очередь симуляции без блокировки.
// Отказы (не больше MaxQueuedPerPlayer команд игрока в очереди, общая
// очередь заполнена) уходят только отправителю как server_busy; повтор
// безопасен.
func (s *Server) enqueueCommand(sess *Session, msg Message) {
	if msg.CmdID == "" || len(msg.CmdID) > MaxCmdIDLen {
		sess.Send(EncodeError(CodeBadRequest, "cmd_id is required, up to 64 characters"))
		return
	}
	var data map[string]interface{}
	if len(msg.Data) > 0 {
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			sess.Send(EncodeCommandError(msg.CmdID, CodeBadRequest, "invalid command data: "+err.Error()))
			return
		}
	}
	// Повтор cmd_id: выполненная команда получает сохранённый ответ, идущая -
	// ответ по готовности; в очередь повтор не попадает.
	switch state, reply := sess.limiter.replies.begin(msg.CmdID, sess); state {
	case replyDone:
		sess.Send(reply)
		return
	case replyPending:
		return
	}
	if !sess.limiter.reserve(s.limits.MaxQueuedPerPlayer) {
		s.rejectBusy(sess, msg.CmdID, "too many pending commands")
		return
	}
	select {
	case s.events <- Event{Kind: EventCommand, Session: sess, Command: Command{ID: msg.CmdID, Name: msg.Cmd, Data: data}}:
	default:
		sess.limiter.release()
		s.rejectBusy(sess, msg.CmdID, "command queue full")
	}
}

// rejectBusy отказывает команде, не дошедшей до очереди: cmd_id забывается,
// чтобы повтор её выполнил, отказ получают отправитель и присоединившиеся.
func (s *Server) rejectBusy(sess *Session, cmdID, reason string) {
	data := EncodeCommandError(cmdID, CodeServerBusy, reason)
	sess.Send(data)
	for _, w := range sess.limiter.replies.forget(cmdID, sess) {
		w.Send(data)
	}
}

// Encode сериализует типизированное сообщение для Session.Send.
func Encode(msgType MessageType, payload interface{}) []byte {
	return EncodeReply(msgType, "", payload)
}

// EncodeReply сериализует ответ на команду cmdID; payload nil - без data.
func EncodeReply(msgType MessageType, cmdID string, payload interface{}) []byte {
	msg := Message{Type: msgType, CmdID: cmdID}
	if payload != nil {
		msg.Data = mustMarshal(payload)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("failed to marshal %s message: %v", msgType, err)
		return []byte(`{"type":"error","data":{"code":"internal","error":"marshal failed"}}`)
	}
	return data
}

// EncodeError сериализует ошибку, не связанную с командой.
func EncodeError(code, errMsg string) []byte {
	return EncodeCommandError("", code, errMsg)
}

// EncodeCommandError сериализует ошибку команды cmdID.
func EncodeCommandError(cmdID, code, errMsg string) []byte {
	return EncodeReply(MessageTypeError, cmdID, ErrorData{Code: code, Error: errMsg})
}

func mustMarshal(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("failed to marshal state: %v", err)
		return json.RawMessage(`{"error":"marshal failed"}`)
	}
	return data
}
