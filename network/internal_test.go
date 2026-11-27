package network

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/state"
)

// egressDriver is a driver that can carry traffic out of the engine, as a
// relay with egress enabled will.
type egressDriver struct {
	*netdriver.Local
	on bool
}

func (e egressDriver) EgressEnabled() bool { return e.on }

func localDriver(t *testing.T) *netdriver.Local {
	t.Helper()
	d, err := netdriver.NewLocal(netdriver.DefaultPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// TestInternalFlag: a network created internal says so in List, Get and
// after a restart; one created the ordinary way does not; a record written
// before the flag existed reads back as not internal.
func TestInternalFlag(t *testing.T) {
	st := &state.Mem{}
	m, _, err := New(Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	in, err := m.CreateWith("sealed", nil, true)
	if err != nil || !in.Internal {
		t.Fatalf("CreateWith internal: %+v, %v", in, err)
	}
	out, _ := m.Create("open", nil)
	if out.Internal {
		t.Fatalf("Create is internal: %+v", out)
	}
	if got, _ := m.Get("sealed"); !got.Internal {
		t.Fatalf("Get lost the flag: %+v", got)
	}
	for _, n := range m.List() {
		if n.Internal != (n.Name == "sealed") {
			t.Errorf("List: %s internal=%v", n.Name, n.Internal)
		}
	}
	m.Close()

	// Written by an older engine: no "internal" key.
	st.Put("network/"+keyName("legacy"), []byte(`{"name":"legacy","id":"`+idFor("legacy")+`","subnet":"10.120.0.0/16","created":"2026-01-01T00:00:00Z"}`))
	m, rep, err := New(Options{Store: st})
	if err != nil || len(rep) != 0 {
		t.Fatal(err, rep)
	}
	defer m.Close()
	if got, err := m.Get("sealed"); err != nil || !got.Internal || got.ID != in.ID {
		t.Fatalf("restart lost the flag: %+v, %v", got, err)
	}
	if got, err := m.Get("open"); err != nil || got.Internal {
		t.Fatalf("restart made open internal: %+v, %v", got, err)
	}
	if got, err := m.Get("legacy"); err != nil || got.Internal {
		t.Fatalf("an old record is internal: %+v, %v", got, err)
	}
}

// TestInternalNoRouteOut: a container on an internal network reaches its
// peers by name and address, and nothing outside the subnet.
func TestInternalNoRouteOut(t *testing.T) {
	m, _, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	n, err := m.CreateWith("sealed", nil, true)
	if err != nil || !n.Internal {
		t.Fatalf("%+v, %v", n, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ep, err := m.Attach(ctx, "sealed", "web", nil)
	if err != nil || ep == nil {
		t.Fatal(ep, err)
	}
	if _, err := ep.Dial(ctx, "tcp", netip.MustParseAddrPort("93.184.216.34:80")); err == nil {
		t.Fatal("an internal network reached a public address")
	}
}

// TestAddDriverInternalRefusesEgress: threat model section 11. An internal
// network cannot be given an egress-capable driver; the refusal leaves no
// network behind.
func TestAddDriverInternalRefusesEgress(t *testing.T) {
	m, _, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	eg := egressDriver{localDriver(t), true}
	if _, err := m.AddDriverWith("sealed-class", "relay", eg, true); !errors.Is(err, ErrInvalid) {
		t.Fatalf("an egress driver on an internal network: %v", err)
	}
	if _, err := m.Get("sealed-class"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the refused network exists: %v", err)
	}
	// The same driver on an ordinary network is fine, and says it is not
	// internal.
	n, err := m.AddDriverWith("open-class", "relay", eg, false)
	if err != nil || n.Internal {
		t.Fatalf("egress driver on an ordinary network: %+v, %v", n, err)
	}
	// A driver with egress switched off, or with no egress notion at all,
	// is allowed on an internal network, and the network is internal.
	for i, d := range []netdriver.NetworkDriver{egressDriver{localDriver(t), false}, localDriver(t)} {
		name := []string{"off", "plain"}[i]
		n, err := m.AddDriverWith(name, "relay", d, true)
		if err != nil || !n.Internal {
			t.Fatalf("%s on an internal network: %+v, %v", name, n, err)
		}
	}
	// AddDriver is the ordinary network.
	if n, err := m.AddDriver("old", "relay", eg); err != nil || n.Internal {
		t.Fatalf("AddDriver: %+v, %v", n, err)
	}
}
