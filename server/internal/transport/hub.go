package transport

import "sort"

// Hub - реестр подключённых сессий. Не потокобезопасен: принадлежит
// горутине симуляции, которая добавляет и убирает сессии по событиям
// EventJoin и EventLeave. Отправка через Session.Send не блокирует.
type Hub struct {
	sessions map[*Session]struct{}
}

// NewHub создаёт пустой реестр.
func NewHub() *Hub {
	return &Hub{sessions: make(map[*Session]struct{})}
}

// Add регистрирует сессию.
func (h *Hub) Add(s *Session) {
	h.sessions[s] = struct{}{}
}

// Remove снимает сессию с учёта.
func (h *Hub) Remove(s *Session) {
	delete(h.sessions, s)
}

// Len возвращает число сессий.
func (h *Hub) Len() int {
	return len(h.sessions)
}

// Sessions возвращает сессии по возрастанию ID.
func (h *Hub) Sessions() []*Session {
	list := make([]*Session, 0, len(h.sessions))
	for s := range h.sessions {
		list = append(list, s)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].ID < list[j].ID })
	return list
}

// Send отправляет сообщение одной сессии: состояние - в слот с затиранием,
// остальное - в очередь уведомлений.
func (h *Hub) Send(s *Session, msgType MessageType, payload interface{}) bool {
	return deliver(s, msgType, Encode(msgType, payload))
}

func deliver(s *Session, msgType MessageType, data []byte) bool {
	if msgType == MessageTypeState {
		return s.SendState(data)
	}
	return s.Send(data)
}

// BroadcastPerPlayer отправляет каждой сессии сообщение её игрока.
// payload вызывается и сериализуется один раз на игрока, даже если у него
// несколько сессий (вкладок).
func (h *Hub) BroadcastPerPlayer(msgType MessageType, payload func(playerID string) interface{}) {
	encoded := make(map[string][]byte)
	for _, s := range h.Sessions() {
		data, ok := encoded[s.PlayerID]
		if !ok {
			data = Encode(msgType, payload(s.PlayerID))
			encoded[s.PlayerID] = data
		}
		deliver(s, msgType, data)
	}
}
