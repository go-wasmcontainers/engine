package network

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// fakeEP is an endpoint whose Dial always succeeds, wherever it is asked to
// go: a network that really does reach the outside.
type fakeEP struct {
	remote bool
	dialed *atomic.Int32
}

func (fakeEP) Addr() netip.Addr { return netip.MustParseAddr("10.9.9.9") }
func (e fakeEP) Listen(context.Context, string, uint16) (net.Listener, error) {
	return nil, errors.New("fake")
}
func (e fakeEP) Dial(context.Context, string, netip.AddrPort) (net.Conn, error) {
	e.dialed.Add(1)
	a, _ := net.Pipe()
	return a, nil
}
func (fakeEP) ListenPacket(context.Context, string, uint16) (net.PacketConn, error) {
	return nil, errors.New("fake")
}
func (e fakeEP) Resolve(context.Context, string) ([]netip.Addr, error) {
	e.dialed.Add(1)
	return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil
}
func (fakeEP) Detach() error  { return nil }
func (e fakeEP) Remote() bool { return e.remote }

// relayLike is a driver that reaches out. It declares its egress through
// EgressEnabled (live state, as a relay learning it from a Welcome would)
// and its endpoints report remote.
type relayLike struct {
	on       *atomic.Bool
	remote   bool
	dialed   *atomic.Int32
	attached *atomic.Int32
}

func newRelayLike(on, remote bool) relayLike {
	b := &atomic.Bool{}
	b.Store(on)
	return relayLike{b, remote, &atomic.Int32{}, &atomic.Int32{}}
}
func (r relayLike) Attach(context.Context, string) (netdriver.Endpoint, error) {
	r.attached.Add(1)
	return fakeEP{r.remote, r.dialed}, nil
}
func (r relayLike) Close() error        { return nil }
func (r relayLike) EgressEnabled() bool { return r.on.Load() }

// silent is a driver that says nothing about egress and reaches out.
type silent struct{ d relayLike }

// EgressEnabled is not a method of silent, and the field is not embedded.
func (s silent) Attach(ctx context.Context, n string) (netdriver.Endpoint, error) {
	return s.d.Attach(ctx, n)
}
func (s silent) Close() error { return nil }

// wrapper decorates a driver through the interface, as a logging wrapper
// would: EgressEnabled is not promoted.
type wrapper struct{ netdriver.NetworkDriver }

var pub = netip.MustParseAddrPort("93.184.216.34:80")

func ctx5(t *testing.T) context.Context {
	c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	return c
}

// Finding 5: the flag, not the topology, is what refuses: the same driver
// reaches out on an ordinary network and is refused on an internal one.
func TestInternalFlagIsWhatRefuses(t *testing.T) {
	m, _, _ := New(Options{})
	defer m.Close()
	d := newRelayLike(false, false)
	d2 := newRelayLike(false, false)
	if _, err := m.AddDriverWith("open", "relay", d2, false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddDriverWith("sealed", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Attach(ctx5(t), "open", "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	sep, err := m.Attach(ctx5(t), "sealed", "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.on.Store(true)
	d2.on.Store(true)
	if c, err := ep.Dial(ctx5(t), "tcp", pub); err != nil {
		t.Fatalf("ordinary network refused a dial: %v", err)
	} else {
		c.Close()
	}
	before := d.dialed.Load()
	c, err := sep.Dial(ctx5(t), "tcp", pub)
	if err == nil {
		c.Close()
		t.Fatal("internal network dialled a public address")
	}
	if !errors.Is(err, ErrEgress) || !errors.Is(err, syscall.ENETUNREACH) {
		t.Fatalf("dial error %v: want ErrEgress and ENETUNREACH", err)
	}
	if d.dialed.Load() != before {
		t.Fatal("the refused dial reached the driver")
	}
	if _, err := sep.Resolve(ctx5(t), "example.com"); !errors.Is(err, ErrEgress) {
		t.Fatalf("resolve on an internal network: %v", err)
	}
}

// Findings 1 and 2: unknown means refused. A driver that does not declare
// "no egress" cannot be on an internal network, and a wrapper cannot hide
// one that does reach out.
func TestInternalRefusesUndeclared(t *testing.T) {
	m, _, _ := New(Options{})
	defer m.Close()
	for name, d := range map[string]netdriver.NetworkDriver{
		"silent":         silent{newRelayLike(false, false)},
		"wrapped-egress": wrapper{newRelayLike(true, false)},
		"wrapped-plain":  wrapper{localDriver(t)},
		"egress-on":      newRelayLike(true, false),
	} {
		if _, err := m.AddDriverWith("s-"+name, "relay", d, true); !errors.Is(err, ErrInvalid) || !errors.Is(err, ErrEgress) {
			t.Errorf("%s accepted on an internal network: %v", name, err)
		}
		if _, err := m.Get("s-" + name); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: the refused network exists", name)
		}
	}
	// A positive declaration of no egress, and an in-engine driver, are fine.
	for name, d := range map[string]netdriver.NetworkDriver{
		"declared-off": newRelayLike(false, false),
		"local":        localDriver(t),
	} {
		if n, err := m.AddDriverWith("ok-"+name, "relay", d, true); err != nil || !n.Internal {
			t.Errorf("%s: %+v, %v", name, n, err)
		}
	}
}

// Finding 3: egress learnt after the add is caught at attach and at every
// dial of an endpoint attached before.
func TestInternalRechecksLive(t *testing.T) {
	m, _, _ := New(Options{})
	defer m.Close()
	d := newRelayLike(false, false)
	if _, err := m.AddDriverWith("sealed", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Attach(ctx5(t), "sealed", "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.on.Store(true) // the Welcome arrives
	if c, err := ep.Dial(ctx5(t), "tcp", pub); err == nil {
		c.Close()
		t.Fatal("an endpoint attached before the flip dialled out")
	}
	if _, err := m.Attach(ctx5(t), "sealed", "db", nil); !errors.Is(err, ErrEgress) {
		t.Fatalf("attach after the flip: %v", err)
	}
	if d.dialed.Load() != 0 {
		t.Fatal("a refused call reached the driver")
	}
	if d.attached.Load() != 1 {
		t.Fatalf("the driver was attached %d times, want 1: a driver that learnt egress is not called", d.attached.Load())
	}
	if _, err := ep.Listen(ctx5(t), "tcp", 80); !errors.Is(err, ErrEgress) {
		t.Fatalf("Listen after the flip: %v", err)
	}
	if _, err := ep.ListenPacket(ctx5(t), "udp", 80); !errors.Is(err, ErrEgress) {
		t.Fatalf("ListenPacket after the flip: %v", err)
	}
}

// An in-engine internal network keeps what Multi routes by: the endpoint's
// Prefix.
func TestInternalLocalKeepsPrefix(t *testing.T) {
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.CreateWith("sealed", nil, true); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Attach(ctx5(t), "sealed", "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, ok := ep.(interface{ Prefix() netip.Prefix })
	if !ok || !p.Prefix().Contains(ep.Addr()) {
		t.Fatalf("the sealed endpoint lost its Prefix: %T", ep)
	}
}

// Remote endpoints are refused whatever the driver declares.
func TestInternalRefusesRemoteEndpoint(t *testing.T) {
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("sealed", "relay", newRelayLike(false, true), true); err != nil {
		t.Fatal(err)
	}
	if ep, err := m.Attach(ctx5(t), "sealed", "web", nil); !errors.Is(err, ErrEgress) || ep != nil {
		t.Fatalf("remote endpoint on an internal network: %v, %v", ep, err)
	}
}

// Finding 4: a name that was internal stays internal for the manager's life.
func TestInternalSurvivesReAdd(t *testing.T) {
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("sealed", "relay", localDriver(t), true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.RemoveDriver("sealed"); err != nil {
		t.Fatal(err)
	}
	if n, err := m.AddDriver("sealed", "relay", newRelayLike(true, true)); err == nil {
		t.Fatalf("same name re-added with egress, internal=%v", n.Internal)
	}
	n, err := m.AddDriver("sealed", "relay", localDriver(t))
	if err != nil || !n.Internal {
		t.Fatalf("re-add of a confined driver: %+v, %v", n, err)
	}
}

// The Options.Drivers path takes the same guard.
func TestOptionsDriversInternal(t *testing.T) {
	if _, _, err := New(Options{Drivers: map[string]netdriver.NetworkDriver{"class": newRelayLike(true, true)}, Internal: map[string]bool{"class": true}}); !errors.Is(err, ErrEgress) {
		t.Fatalf("egress driver internal through Options: %v", err)
	}
	if _, _, err := New(Options{Drivers: map[string]netdriver.NetworkDriver{"class": silent{newRelayLike(false, false)}}, Internal: map[string]bool{"class": true}}); !errors.Is(err, ErrEgress) {
		t.Fatalf("undeclared driver internal through Options: %v", err)
	}
	if _, _, err := New(Options{Internal: map[string]bool{"nope": true}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("internal flag for an unknown driver: %v", err)
	}
	d := newRelayLike(false, false)
	m, _, err := New(Options{Drivers: map[string]netdriver.NetworkDriver{"class": d}, Internal: map[string]bool{"class": true}})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if n, _ := m.Get("class"); !n.Internal {
		t.Fatalf("not internal: %+v", n)
	}
	ep, err := m.Attach(ctx5(t), "class", "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	d.on.Store(true)
	if c, err := ep.Dial(ctx5(t), "tcp", pub); err == nil {
		c.Close()
		t.Fatal("dial out of an internal Options driver")
	}
}
