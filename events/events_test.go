package events

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestBus(t *testing.T) {
	b := New()
	t0 := time.Unix(1000, 0)
	tick := t0
	b.now = func() time.Time { tick = tick.Add(time.Second); return tick }
	b.Publish(Event{Type: Container, Action: "create", ID: "a"})
	b.Publish(Event{Type: Image, Action: "pull", ID: "hello"})
	past, s := b.Subscribe(t0, func(e Event) bool { return e.Type == Container })
	defer s.Close()
	if len(past) != 1 || past[0].ID != "a" {
		t.Fatalf("%+v", past)
	}
	b.Publish(Event{Type: Image, Action: "delete", ID: "hello"})
	b.Publish(Event{Type: Container, Action: "start", ID: "a"})
	if e := <-s.C; e.Action != "start" {
		t.Fatalf("%+v", e)
	}
	// No since: nothing past.
	if past, s2 := b.Subscribe(time.Time{}, nil); len(past) != 0 {
		t.Fatal(past)
	} else {
		s2.Close()
		s2.Close() // twice is fine
	}
}

func TestHistoryIsBounded(t *testing.T) {
	b := New()
	for i := 0; i < History+10; i++ {
		b.Publish(Event{Type: Container, Action: "x", ID: fmt.Sprint(i)})
	}
	past, s := b.Subscribe(time.Unix(1, 0), nil)
	s.Close()
	if len(past) != History || past[0].ID != "10" || past[len(past)-1].ID != fmt.Sprint(History+9) {
		t.Fatalf("%d events, first %s", len(past), past[0].ID)
	}
}

// A subscriber that does not read is cut off; the publisher never blocks.
func TestSlowSubscriberIsCutOff(t *testing.T) {
	b := New()
	_, slow := b.Subscribe(time.Time{}, nil)
	_, fast := b.Subscribe(time.Time{}, nil)
	var wg sync.WaitGroup
	wg.Add(1)
	got := 0
	go func() {
		defer wg.Done()
		for range fast.C {
			got++
		}
	}()
	done := make(chan struct{})
	go func() {
		for i := 0; i < Queue*4; i++ {
			b.Publish(Event{Type: Container, Action: "x"})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber")
	}
	fast.Close()
	wg.Wait()
	if !slow.Lost() {
		t.Fatal("slow subscriber not cut off")
	}
	n := 0
	for range slow.C {
		n++
	}
	if n != Queue {
		t.Fatalf("slow got %d, want its queue (%d) then the end", n, Queue)
	}
	if fast.Lost() {
		t.Fatalf("fast subscriber cut off after %d", got)
	}
}
