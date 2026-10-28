// Package events is the engine's event bus: what happened to containers,
// images and volumes, as GET /events streams it (Docker's EventMessage:
// Type, Action, Actor{ID, Attributes}, time). The bus keeps a bounded
// history for `since` and gives each subscriber a bounded queue; a
// subscriber that falls that far behind is cut off (its channel closed,
// Lost set) rather than slowing the engine down.
package events

import (
	"sync"
	"time"
)

// Types of objects that emit events.
const (
	Container = "container"
	Image     = "image"
	Volume    = "volume"
	Network   = "network"
)

// Event is one thing that happened.
type Event struct {
	Type, Action string
	// ID is the actor: a container ID, an image ID or reference, a
	// volume name.
	ID         string
	Attributes map[string]string
	Time       time.Time
}

const (
	// History is how many past events the bus keeps for `since`.
	History = 1024
	// Queue is how many events a subscriber may be behind.
	Queue = 256
)

// Bus fans events out. The zero value is not usable; use New.
type Bus struct {
	mu   sync.Mutex
	past []Event // ring, oldest first once full
	head int
	full bool
	subs map[*Sub]struct{}
	now  func() time.Time
}

// New returns an empty bus.
func New() *Bus {
	return &Bus{past: make([]Event, History), subs: map[*Sub]struct{}{}, now: time.Now}
}

// Sub is a subscription.
type Sub struct {
	C      <-chan Event
	c      chan Event
	filter func(Event) bool
	lost   bool
	b      *Bus
}

// Lost reports whether the subscription was cut off for falling behind.
func (s *Sub) Lost() bool {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	return s.lost
}

// Close ends the subscription.
func (s *Sub) Close() {
	s.b.mu.Lock()
	defer s.b.mu.Unlock()
	if _, ok := s.b.subs[s]; ok {
		delete(s.b.subs, s)
		close(s.c)
	}
}

// Publish records e (stamping its time if unset) and delivers it.
func (b *Bus) Publish(e Event) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if e.Time.IsZero() {
		e.Time = b.now()
	}
	b.past[b.head] = e
	b.head = (b.head + 1) % len(b.past)
	if b.head == 0 {
		b.full = true
	}
	for s := range b.subs {
		if s.filter != nil && !s.filter(e) {
			continue
		}
		select {
		case s.c <- e:
		default:
			s.lost = true
			delete(b.subs, s)
			close(s.c)
		}
	}
}

// Subscribe returns the kept events at or after since that pass filter
// (nil: all), and a subscription for what comes next; nothing is lost or
// repeated between the two.
func (b *Bus) Subscribe(since time.Time, filter func(Event) bool) ([]Event, *Sub) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var past []Event
	if !since.IsZero() {
		n := b.head
		start := 0
		if b.full {
			n, start = len(b.past), b.head
		}
		for i := 0; i < n; i++ {
			e := b.past[(start+i)%len(b.past)]
			if !e.Time.Before(since) && (filter == nil || filter(e)) {
				past = append(past, e)
			}
		}
	}
	c := make(chan Event, Queue)
	s := &Sub{C: c, c: c, filter: filter, b: b}
	b.subs[s] = struct{}{}
	return past, s
}
