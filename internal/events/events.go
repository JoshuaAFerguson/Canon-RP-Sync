// Package events is a small in-process publish/subscribe bus. The watcher
// publishes what it is doing; the HTTP API streams it to the web UI and the
// companion apps over server-sent events.
package events

import (
	"sync"
	"time"
)

// Kind classifies an event.
type Kind string

const (
	// KindCamera reports a change in camera connectivity.
	KindCamera Kind = "camera"
	// KindImport reports import progress.
	KindImport Kind = "import"
	// KindRemote reports remote-access (tunnel) state.
	KindRemote Kind = "remote"
	// KindPairing reports device pairing activity.
	KindPairing Kind = "pairing"
	// KindError reports a failure worth showing the user.
	KindError Kind = "error"
)

// Event is one thing that happened.
type Event struct {
	Kind    Kind      `json:"kind"`
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
	// Data carries structured detail, e.g. the manifest entry for an import.
	Data any `json:"data,omitempty"`
}

// historySize is how many recent events a late subscriber can catch up on.
const historySize = 100

// Bus fans events out to subscribers. A slow subscriber is skipped rather than
// allowed to block the watcher, so a stalled browser tab cannot stop imports.
type Bus struct {
	mu      sync.RWMutex
	subs    map[int]chan Event
	nextID  int
	history []Event
}

// NewBus returns an empty bus.
func NewBus() *Bus {
	return &Bus{subs: map[int]chan Event{}}
}

// Publish delivers an event to every subscriber and records it in the history.
func (b *Bus) Publish(ev Event) {
	if ev.Time.IsZero() {
		ev.Time = time.Now()
	}

	b.mu.Lock()
	b.history = append(b.history, ev)
	if len(b.history) > historySize {
		b.history = b.history[len(b.history)-historySize:]
	}
	subs := make([]chan Event, 0, len(b.subs))
	for _, ch := range b.subs {
		subs = append(subs, ch)
	}
	b.mu.Unlock()

	for _, ch := range subs {
		select {
		case ch <- ev:
		default: // subscriber is behind; drop rather than block the producer
		}
	}
}

// Publishf is a convenience for message-only events.
func (b *Bus) Publishf(kind Kind, format string, args ...any) {
	b.Publish(Event{Kind: kind, Message: sprintf(format, args...)})
}

// Subscribe returns a channel of future events and a function to unsubscribe.
// The channel is buffered; callers must keep reading or they will miss events.
func (b *Bus) Subscribe(buffer int) (<-chan Event, func()) {
	if buffer <= 0 {
		buffer = 32
	}
	ch := make(chan Event, buffer)

	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.subs[id] = ch
	b.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			b.mu.Lock()
			delete(b.subs, id)
			b.mu.Unlock()
			close(ch)
		})
	}
}

// Recent returns up to n of the most recent events, oldest first.
func (b *Bus) Recent(n int) []Event {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if n <= 0 || n > len(b.history) {
		n = len(b.history)
	}
	out := make([]Event, n)
	copy(out, b.history[len(b.history)-n:])
	return out
}

// Subscribers reports how many live subscriptions exist, for tests and status.
func (b *Bus) Subscribers() int {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return len(b.subs)
}
