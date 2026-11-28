package network

import (
	"errors"
	"testing"

	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/state"
)

type failKeys struct{ state.Store }

func (f failKeys) Keys(p string) ([]string, error) {
	if p == "sealed/" {
		return nil, errors.New("unreadable")
	}
	return f.Store.Keys(p)
}

type failGet struct{ state.Store }

func (failGet) Get(string) ([]byte, error) { return nil, errors.New("unreadable") }

// The set that only grows is read or the engine does not start; a record that
// does not match its key is ignored; a name already recorded is not written
// again.
func TestSealedStoreEdges(t *testing.T) {
	st := &state.Mem{}
	m, _, err := New(Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddDriverWith("sealed", "relay", localDriver(t), true); err != nil {
		t.Fatal(err)
	}
	m.Close()
	if _, _, err := New(Options{Store: failKeys{st}}); err == nil {
		t.Fatal("started with an unreadable sealed set (Keys)")
	}
	// Already recorded: no second Put (the store would refuse it).
	if _, _, err := New(Options{Store: failPut{st}, Drivers: map[string]netdriver.NetworkDriver{"sealed": localDriver(t)}}); err != nil {
		t.Fatalf("a recorded name was written again: %v", err)
	}
	// A new internal name through Options whose record cannot be written.
	if _, _, err := New(Options{Store: failPut{&state.Mem{}}, Internal: map[string]bool{"fresh": true}, Drivers: map[string]netdriver.NetworkDriver{"fresh": localDriver(t)}}); err == nil {
		t.Fatal("an internal Options driver whose flag could not be stored was accepted")
	}
	// A forged record: the value names "y" under the key of "x".
	if err := st.Put(sealedKey("x"), []byte("y")); err != nil {
		t.Fatal(err)
	}
	m2, _, err := New(Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	// The key is the authority: "x" is sealed whatever the value says, and
	// the value does not seal the name it names.
	if _, err := m2.AddDriver("x", "relay", newRelayLike(true, true)); err == nil {
		t.Fatal("x was not sealed by its own key")
	}
	if _, err := m2.AddDriver("y", "relay", newRelayLike(true, true)); err != nil {
		t.Fatalf("y was sealed by a forged record: %v", err)
	}
}

// One sealed record that cannot be read, or does not parse, neither stops the
// engine nor opens its network: the name whose key it is stays internal, and
// the damage is reported.
func TestSealedDamagedRecordFailsClosed(t *testing.T) {
	for name, wrap := range map[string]func(state.Store) state.Store{
		"unreadable": func(s state.Store) state.Store { return failGet{s} },
		"garbage":    func(s state.Store) state.Store { s.Put(sealedKey("sealed"), []byte("\x00\xff")); return s },
	} {
		st := &state.Mem{}
		m, _, err := New(Options{Store: st})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := m.AddDriverWith("sealed", "relay", localDriver(t), true); err != nil {
			t.Fatal(err)
		}
		m.Close()
		m2, repaired, err := New(Options{Store: wrap(st)})
		if err != nil {
			t.Fatalf("%s: the engine did not start: %v", name, err)
		}
		if len(repaired) == 0 {
			t.Fatalf("%s: damage not reported", name)
		}
		if _, err := m2.AddDriver("sealed", "relay", newRelayLike(true, true)); err == nil {
			t.Fatalf("%s: the network opened", name)
		}
		if _, _, err := New(Options{Store: wrap(st), Drivers: map[string]netdriver.NetworkDriver{"sealed": newRelayLike(true, true)}}); !errors.Is(err, ErrEgress) {
			t.Fatalf("%s: an egress driver under the damaged name: %v", name, err)
		}
		m2.Close()
	}
}

type failPut struct{ state.Store }

func (failPut) Put(string, []byte) error { return errors.New("disk full") }

// A name that was internal stays so across an engine restart: the set is in
// the Store, not only in memory.
func TestInternalSurvivesRestart(t *testing.T) {
	st := &state.Mem{}
	m, _, err := New(Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddDriverWith("sealed", "relay", localDriver(t), true); err != nil {
		t.Fatal(err)
	}
	m.Close()

	m2, _, err := New(Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer m2.Close()
	if n, err := m2.AddDriver("sealed", "relay", newRelayLike(true, true)); err == nil {
		t.Fatalf("re-added with egress after a restart, internal=%v", n.Internal)
	}
	n, err := m2.AddDriver("sealed", "relay", localDriver(t))
	if err != nil || !n.Internal {
		t.Fatalf("re-add of a confined driver after a restart: %+v, %v", n, err)
	}
	// The Options.Drivers path too: the flag is not repeated, the Store holds it.
	if _, _, err := New(Options{Store: st, Drivers: map[string]netdriver.NetworkDriver{"sealed": newRelayLike(true, true)}}); !errors.Is(err, ErrEgress) {
		t.Fatalf("an egress driver under a name that was internal: %v", err)
	}
	// A store that cannot record the name refuses the internal network.
	m3, _, err := New(Options{Store: failPut{st}})
	if err != nil {
		t.Fatal(err)
	}
	defer m3.Close()
	if _, err := m3.AddDriverWith("other", "relay", localDriver(t), true); err == nil {
		t.Fatal("an internal network whose flag could not be stored was accepted")
	}
}

// A damaged record is rewritten once: the report does not repeat at every
// start, and its network stays internal.
func TestSealedDamagedRecordReportedOnce(t *testing.T) {
	st := &state.Mem{}
	if err := st.Put(sealedKey("sealed"), []byte("\x00\xff")); err != nil {
		t.Fatal(err)
	}
	m, repaired, err := New(Options{Store: st})
	if err != nil || len(repaired) != 1 {
		t.Fatalf("first start: %v, %v", repaired, err)
	}
	m.Close()
	m2, repaired, err := New(Options{Store: st})
	if err != nil || len(repaired) != 0 {
		t.Fatalf("second start reports again: %v, %v", repaired, err)
	}
	defer m2.Close()
	if _, err := m2.AddDriver("sealed", "relay", newRelayLike(true, true)); err == nil {
		t.Fatal("the network opened after the rewrite")
	}
}
