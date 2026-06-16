package gateway

import (
	"sync"

	"github.com/yurifrl/agents/internal/a2a"
)

// broker fans out a task's A2A events to live subscribers and keeps a replay
// buffer so a late or reconnecting subscriber sees what it missed.
type broker struct {
	mu       sync.Mutex
	history  []a2a.Event
	subs     map[int]chan a2a.Event
	nextID   int
	finished bool
}

func newBroker() *broker {
	return &broker{subs: make(map[int]chan a2a.Event)}
}

// publish records an event and delivers it to all live subscribers.
func (b *broker) publish(ev a2a.Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.history = append(b.history, ev)
	if ev.Type == a2a.EventResult || ev.Type == a2a.EventError {
		b.finished = true
	}
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// subscribe returns a channel pre-loaded with history, plus an unsubscribe func.
// If the task already finished, the channel is closed after replay.
func (b *broker) subscribe() (<-chan a2a.Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ch := make(chan a2a.Event, len(b.history)+16)
	for _, ev := range b.history {
		ch <- ev
	}
	if b.finished {
		close(ch)
		return ch, func() {}
	}

	id := b.nextID
	b.nextID++
	b.subs[id] = ch

	return ch, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if c, ok := b.subs[id]; ok {
			delete(b.subs, id)
			close(c)
		}
	}
}

// close terminates all live subscriptions.
func (b *broker) close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for id, ch := range b.subs {
		delete(b.subs, id)
		close(ch)
	}
}
