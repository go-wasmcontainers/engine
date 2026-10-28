package cli

import (
	"context"
	"errors"
	"testing"
)

// TestFollowSize: only a real change of the window is sent, the first
// one compared with the size the exec started with; failed reads and
// failed sends do not count as sent.
func TestFollowSize(t *testing.T) {
	sizes := [][3]int{{80, 24, 0}, {80, 24, 0}, {100, 30, 0}, {100, 30, 0}, {0, 0, 1}, {120, 40, 0}, {120, 40, 0}}
	i := 0
	get := func() (int, int, error) {
		s := sizes[i]
		i++
		if s[2] != 0 {
			return 0, 0, errors.New("no terminal")
		}
		return s[0], s[1], nil
	}
	var sent [][2]uint16
	failNext := false
	resize := func(h, w uint16) error {
		if failNext {
			failNext = false
			return errors.New("busy")
		}
		sent = append(sent, [2]uint16{h, w})
		return nil
	}
	var fire func()
	changes := func(_ context.Context, fn func()) { fire = fn }
	followSize(context.Background(), get, resize, []uint16{24, 80}, changes)
	for range 4 {
		fire()
	}
	fire() // read error
	failNext = true
	fire() // 120x40, send fails
	fire() // 120x40 again: retried
	want := [][2]uint16{{30, 100}, {40, 120}}
	if len(sent) != len(want) || sent[0] != want[0] || sent[1] != want[1] {
		t.Errorf("sent %v, want %v", sent, want)
	}
}
