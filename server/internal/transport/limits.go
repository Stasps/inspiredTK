package transport

import (
	"sync"
	"time"
)

// Limits - пределы сетевого края (documentation/architecture.md, «Очереди и
// переполнение»). Значения стартовые, уточняются нагрузочным тестом.
type Limits struct {
	// QueueSize - общая очередь событий симуляции (команды, вход и выход).
	QueueSize int
	// CommandRate и CommandBurst - token bucket игрока: команд в секунду и
	// запас. Делится между всеми сессиями (вкладками) игрока.
	CommandRate  float64
	CommandBurst int
	// MaxQueuedPerPlayer - сколько команд игрока может ждать в общей очереди.
	MaxQueuedPerPlayer int
	// MaxRateStrikes - отказов rate_limited подряд, после которых соединение
	// рвётся.
	MaxRateStrikes int
	// OutboxSize - ёмкость очереди уведомлений сессии.
	OutboxSize int
	// WriteTimeout - предельное время записи одного сообщения в сокет.
	WriteTimeout time.Duration
	// PingInterval - интервал ping от сервера; ReadTimeout - сколько сессия
	// может молчать (ни сообщений, ни pong), прежде чем её закроют.
	PingInterval time.Duration
	ReadTimeout  time.Duration
	// ReplyMemory - сколько последних cmd_id игрока помнит сервер, чтобы
	// повтор получил тот же ответ без повторного выполнения.
	ReplyMemory int
}

// DefaultLimits возвращает стартовые пределы.
func DefaultLimits() Limits {
	return Limits{
		QueueSize:    1024,
		CommandRate:  10,
		CommandBurst: 10,
		// Не меньше CommandBurst: иначе залп в пределах запаса token bucket
		// упирается в server_busy даже на свободном сервере (замер 1.2a3:
		// горутина чтения принимает команды быстрее, чем цикл их вынимает).
		MaxQueuedPerPlayer: 10,
		MaxRateStrikes:     20,
		OutboxSize:         OutboxSize,
		WriteTimeout:       WriteTimeout,
		PingInterval:       PingInterval,
		ReadTimeout:        ReadTimeout,
		ReplyMemory:        64,
	}
}

// playerLimiter - входящие пределы одного игрока: token bucket частоты
// команд, число его команд в общей очереди и память ответов по cmd_id.
// Общий для сессий игрока, поэтому под собственной блокировкой (это
// состояние сетевого края, а не мира).
type playerLimiter struct {
	mu     sync.Mutex
	tokens float64
	last   time.Time
	queued int

	replies *replyMemory
}

// allow списывает один жетон, если он есть.
func (l *playerLimiter) allow(now time.Time, rate float64, burst int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if elapsed := now.Sub(l.last).Seconds(); elapsed > 0 {
		l.tokens = min(float64(burst), l.tokens+elapsed*rate)
	}
	l.last = now
	if l.tokens < 1 {
		return false
	}
	l.tokens--
	return true
}

// reserve занимает место игрока в общей очереди.
func (l *playerLimiter) reserve(maxQueued int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.queued >= maxQueued {
		return false
	}
	l.queued++
	return true
}

// release освобождает место игрока в общей очереди.
func (l *playerLimiter) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.queued > 0 {
		l.queued--
	}
}

// limiterRegistry выдаёт сессиям ограничитель их игрока. Запись живёт, пока
// жив процесс: память ответов (replies) переживает переподключение, а
// переподключение не сбрасывает лимит частоты. Размер ограничен числом
// игроков; в снапшот запись не попадает.
type limiterRegistry struct {
	mu         sync.Mutex
	byPlayer   map[string]*playerLimiter
	burst      int
	replyLimit int
}

func newLimiterRegistry(burst, replyLimit int) *limiterRegistry {
	return &limiterRegistry{byPlayer: make(map[string]*playerLimiter), burst: burst, replyLimit: replyLimit}
}

// acquire возвращает ограничитель игрока; новый начинается с полным запасом.
func (r *limiterRegistry) acquire(playerID string, now time.Time) *playerLimiter {
	r.mu.Lock()
	defer r.mu.Unlock()
	l, ok := r.byPlayer[playerID]
	if !ok {
		l = &playerLimiter{tokens: float64(r.burst), last: now, replies: newReplyMemory(r.replyLimit)}
		r.byPlayer[playerID] = l
	}
	return l
}
