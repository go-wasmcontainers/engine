package network

import (
	"sync"
	"testing"
)

func tripCount(m *Manager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.trips)
}

// A runtime-added network leaves no trip entry behind once removed.
func TestTripsDroppedByRemoveDriver(t *testing.T) {
	ctx := ctx5(t)
	m, _, _ := New(Options{})
	defer m.Close()
	for i := 0; i < 5; i++ {
		if _, err := m.AddDriverWith("rt", "relay", localDriver(t), true); err != nil {
			t.Fatal(err)
		}
		ep, err := m.Attach(ctx, "rt", "c", nil)
		if err != nil {
			t.Fatal(err)
		}
		ep.Detach()
		d, err := m.RemoveDriver("rt")
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
	}
	if n := tripCount(m); n != 0 {
		t.Fatalf("%d trip entries after 5 add/remove cycles, want 0", n)
	}
}

// RemoveDriver racing Attach never leaves an entry for the removed network.
func TestTripsNotRecreatedByRacingAttach(t *testing.T) {
	ctx := ctx5(t)
	m, _, _ := New(Options{})
	defer m.Close()
	for i := 0; i < 20; i++ {
		if _, err := m.AddDriverWith("rc", "relay", localDriver(t), true); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			if ep, err := m.Attach(ctx, "rc", "c", nil); err == nil {
				ep.Detach()
			}
		}()
		go func() {
			defer wg.Done()
			if d, err := m.RemoveDriver("rc"); err == nil {
				d.Close()
			}
		}()
		wg.Wait()
		if n := tripCount(m); n != 0 {
			t.Fatalf("round %d: %d trip entries for a removed network", i, n)
		}
	}
}
