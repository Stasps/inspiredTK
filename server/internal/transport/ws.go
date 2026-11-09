// Package transport provides WebSocket communication between server and clients.
package transport

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
)

// DefaultWebSocketPort is the default port for WebSocket connections.
const DefaultWebSocketPort = 8080

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
)

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

// Server handles WebSocket connections and queues client commands.
// It is safe for concurrent use; commands are consumed via CommandChan
// by the single engine goroutine.
type Server struct {
	upgrader websocket.Upgrader
	clients  map[*websocket.Conn]bool
	mu       sync.RWMutex
	cmdChan  chan Command
}

// NewServer creates a new WebSocket server.
func NewServer() *Server {
	return &Server{
		upgrader: websocket.Upgrader{
			CheckOrigin: func(r *http.Request) bool {
				// Allow all origins for development
				return true
			},
		},
		clients: make(map[*websocket.Conn]bool),
		cmdChan: make(chan Command, CommandQueueSize),
	}
}

// CommandChan returns the channel of queued client commands.
func (s *Server) CommandChan() <-chan Command {
	return s.cmdChan
}

// HandleWebSocket handles an incoming WebSocket connection.
func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	s.addClient(conn)
	defer s.removeClient(conn)

	log.Printf("client connected: %s", conn.RemoteAddr())

	for {
		var msg Message
		if err := conn.ReadJSON(&msg); err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("WebSocket error: %v", err)
			}
			break
		}

		if msg.Type == MessageTypeCommand {
			s.handleCommand(conn, msg)
		}
	}

	log.Printf("client disconnected: %s", conn.RemoteAddr())
}

// handleCommand queues a client command, rejecting it if the queue is full.
func (s *Server) handleCommand(conn *websocket.Conn, msg Message) {
	var data map[string]interface{}
	if len(msg.Data) > 0 {
		if err := json.Unmarshal(msg.Data, &data); err != nil {
			s.sendError(conn, "invalid command data: "+err.Error())
			return
		}
	}
	select {
	case s.cmdChan <- Command{Name: msg.Cmd, Data: data}:
	default:
		s.sendError(conn, "command queue full")
	}
}

// BroadcastState sends the current state to all connected clients.
func (s *Server) BroadcastState(state interface{}) {
	s.Broadcast(MessageTypeState, state)
}

// Broadcast sends a typed message to all connected clients.
func (s *Server) Broadcast(msgType MessageType, payload interface{}) {
	msg := Message{Type: msgType, Data: mustMarshal(payload)}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for conn := range s.clients {
		if err := conn.WriteJSON(msg); err != nil {
			log.Printf("failed to send %s to client: %v", msgType, err)
		}
	}
}

func mustMarshal(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("failed to marshal state: %v", err)
		return json.RawMessage(`{"error":"marshal failed"}`)
	}
	return data
}

// addClient registers a new client connection.
func (s *Server) addClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clients[conn] = true
}

// removeClient unregisters a client connection.
func (s *Server) removeClient(conn *websocket.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clients, conn)
}

// sendError sends an error message to a client.
func (s *Server) sendError(conn *websocket.Conn, errMsg string) {
	msg := Message{
		Type: MessageTypeError,
		Data: mustMarshal(map[string]string{"error": errMsg}),
	}
	if err := conn.WriteJSON(msg); err != nil {
		log.Printf("failed to send error to client: %v", err)
	}
}
