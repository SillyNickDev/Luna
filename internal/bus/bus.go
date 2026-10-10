// Package bus is the in-process event bus: one bounded queue per subscriber,
// so a slow subscriber drops its own events instead of stalling the others.
package bus

import (
	"sync"
	"sync/atomic"

	"luna/internal/event"
)

// ponytail: one channel per subscriber, not the spec's shared ring buffer; switch if the
// replay benchmark shows per-subscriber copies cost too much.
type Bus struct {
	mu     sync.RWMutex
	all    map[*Sub]struct{}
	byType map[string]map[*Sub]struct{}
}

type Sub struct {
	C      <-chan event.Event
	c      chan event.Event
	types  []string
	lagged atomic.Uint64
	bus    *Bus
}

func New() *Bus {
	return &Bus{all: map[*Sub]struct{}{}, byType: map[string]map[*Sub]struct{}{}}
}

// Subscribe returns a subscriber for the given event types, or for every event if none are given.
func (b *Bus) Subscribe(size int, types ...string) *Sub {
	c := make(chan event.Event, size)
	s := &Sub{C: c, c: c, types: types, bus: b}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(types) == 0 {
		b.all[s] = struct{}{}
	}
	for _, t := range types {
		if b.byType[t] == nil {
			b.byType[t] = map[*Sub]struct{}{}
		}
		b.byType[t][s] = struct{}{}
	}
	return s
}

// Publish never blocks: a full subscriber queue drops the event and counts it as lagged.
func (b *Bus) Publish(ev event.Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for s := range b.all {
		s.offer(ev)
	}
	for s := range b.byType[ev.Type] {
		s.offer(ev)
	}
}

func (s *Sub) offer(ev event.Event) {
	select {
	case s.c <- ev:
	default:
		s.lagged.Add(1)
	}
}

// Lagged is how many events this subscriber missed because its queue was full.
func (s *Sub) Lagged() uint64 { return s.lagged.Load() }

// Close unsubscribes and closes C once queued events are read. Call it once.
func (s *Sub) Close() {
	b := s.bus
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.all, s)
	for _, t := range s.types {
		delete(b.byType[t], s)
	}
	close(s.c)
}
