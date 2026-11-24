package transport

import (
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// salvoResult - итог залпа: сколько команд получили ack и server_busy.
type salvoResult struct{ acks, busy int }

// runSalvo шлёт залп из n команд подряд по настоящему WebSocket, пока
// «цикл симуляции» (как в main.go: Taken, затем Reply) тратит cost на каждую
// команду, и считает ответы. Лимит частоты поднят, чтобы мерить только очередь.
func runSalvo(t *testing.T, n int, cost time.Duration) salvoResult {
	t.Helper()
	srv, url, _ := startLimitedServer(t, func(l *Limits) {})
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			case ev := <-srv.Events():
				if ev.Kind != EventCommand {
					continue
				}
				ev.Taken()
				time.Sleep(cost)
				ev.Reply(EncodeReply(MessageTypeAck, ev.Command.ID, nil), true)
			}
		}
	}()
	defer func() { close(stop); wg.Wait() }()

	c := dial(t, url+"?player=alice")
	for range n {
		sendCommand(t, c, "noop", nil)
	}
	var res salvoResult
	for range n {
		msg := readMessage(t, c)
		if msg.Type == MessageTypeAck {
			res.acks++
			continue
		}
		var data ErrorData
		_ = json.Unmarshal(msg.Data, &data)
		if data.Code == CodeServerBusy {
			res.busy++
		}
	}
	return res
}

// Замер 1.2a3: залп в пределах запаса token bucket (10) при стартовых
// пределах не должен давать server_busy, даже если цикл симуляции медленный.
// Со старым лимитом 4 мест залп из 5 команд давал server_busy на свободном
// сервере (горутина чтения принимает команды быстрее, чем цикл их вынимает).
func TestSalvoServerBusy(t *testing.T) {
	for _, tc := range []struct {
		n    int
		cost time.Duration
	}{
		{5, 0}, {10, 0}, {10, time.Millisecond}, {10, 50 * time.Millisecond},
	} {
		res := runSalvo(t, tc.n, tc.cost)
		t.Logf("залп %d команд, цикл %v на команду: ack=%d server_busy=%d", tc.n, tc.cost, res.acks, res.busy)
		if res.busy != 0 || res.acks != tc.n {
			t.Errorf("залп %d при %v на команду: ack=%d server_busy=%d, want все ack", tc.n, tc.cost, res.acks, res.busy)
		}
	}
}

// Реальный залп сверх запаса token bucket отсекает rate_limited, а не
// server_busy: очередь игрока не уже запаса.
func TestDefaultQueuePlacesCoverBurst(t *testing.T) {
	l := DefaultLimits()
	if l.MaxQueuedPerPlayer < l.CommandBurst {
		t.Fatalf("MaxQueuedPerPlayer %d < CommandBurst %d", l.MaxQueuedPerPlayer, l.CommandBurst)
	}
}
