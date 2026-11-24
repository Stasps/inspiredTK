// Package transport provides WebSocket communication between server and clients.
package transport

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"
	"sync/atomic"

	"github.com/gorilla/websocket"
)

// CommandQueueSize is the maximum number of pending client commands.
const CommandQueueSize = 100

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
)

// playerIDPattern - допустимый id игрока в параметре подключения.
var playerIDPattern = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// ValidPlayerID сообщает, допустим ли id игрока.
func ValidPlayerID(id string) bool {
	return playerIDPattern.MatchString(id)
}

// Message represents a WebSocket message.
type Message struct {
	Type MessageType     `json:"type"`
	Cmd  string          `json:"cmd,omitempty"`
	Data json.RawMessage `json:"data,omitempty"`
}

// Command represents a client command queued for execution by the world actor.
type Command struct {
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

// Server принимает WebSocket-соединения: на каждое - сессия с горутиной
// чтения (HandleWebSocket) и горутиной записи (Session.writeLoop).
// События сессий читает единственная горутина симуляции через Events.
type Server struct {
	upgrader   websocket.Upgrader
	events     chan Event
	lastID     atomic.Uint64
	outboxSize int
}

// NewServer creates a new WebSocket server.
func NewServer() *Server {
	return newServer(CommandQueueSize, OutboxSize)
}

func newServer(queueSize, outboxSize int) *Server {
	return &Server{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// Allow all origins for development
				return true
			},
		},
		events:     make(chan Event, queueSize),
		outboxSize: outboxSize,
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
	sess := newSession(s.lastID.Add(1), playerID, conn, s.outboxSize)
	log.Printf("session %d connected: player %q from %s", sess.ID, playerID, conn.RemoteAddr())
	s.serve(sess)
	log.Printf("session %d disconnected", sess.ID)
}

// serve ведёт сессию от подключения до отключения. Join и Leave ставятся в
// очередь с ожиданием: блокируется только горутина этой сессии, а не
// симуляция, и цикл симуляции не теряет ни одной сессии.
func (s *Server) serve(sess *Session) {
	s.events <- Event{Kind: EventJoin, Session: sess}
	s.readLoop(sess)
	sess.Close()
	s.events <- Event{Kind: EventLeave, Session: sess}
}

// readLoop читает сообщения до ошибки чтения или закрытия сессии.
func (s *Server) readLoop(sess *Session) {
	for {
		_, data, err := sess.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure, websocket.CloseNormalClosure, websocket.CloseNoStatusReceived) {
				log.Printf("session %d: read: %v", sess.ID, err)
			}
			return
		}
		var msg Message
		if err := json.Unmarshal(data, &msg); err != nil {
			sess.Send(EncodeError("invalid message: " + err.Error()))
			continue
		}
		if msg.Type == MessageTypeCommand {
			s.enqueueCommand(sess, msg)
		}
	}
}

// enqueueCommand ставит команду в очередь симуляции без блокировки;
// при переполнении отказ уходит только отправителю.
func (s *Server) enqueueCommand(sess *Session, msg Message) {
	var data map[string]interface{}
	if len(msg.Data) > 0 {
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			sess.Send(EncodeError("invalid command data: " + err.Error()))
			return
		}
	}
	select {
	case s.events <- Event{Kind: EventCommand, Session: sess, Command: Command{Name: msg.Cmd, Data: data}}:
	default:
		sess.Send(EncodeError("command queue full"))
	}
}

// Encode сериализует типизированное сообщение для Session.Send.
func Encode(msgType MessageType, payload interface{}) []byte {
	data, err := json.Marshal(Message{Type: msgType, Data: mustMarshal(payload)})
	if err != nil {
		log.Printf("failed to marshal %s message: %v", msgType, err)
		return []byte(`{"type":"error","data":{"error":"marshal failed"}}`)
	}
	return data
}

// EncodeError сериализует сообщение об ошибке.
func EncodeError(errMsg string) []byte {
	return Encode(MessageTypeError, map[string]string{"error": errMsg})
}

func mustMarshal(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("failed to marshal state: %v", err)
		return json.RawMessage(`{"error":"marshal failed"}`)
	}
	return data
}
