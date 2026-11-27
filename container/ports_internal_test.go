package container

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"

	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/portmap"
)

type countHost struct {
	portmap.Host
	n atomic.Int32
}

func (h *countHost) Listen(ctx context.Context, network string, a netip.AddrPort) (net.Listener, error) {
	h.n.Add(1)
	return h.Host.Listen(ctx, network, a)
}

// Docker publishes nothing for a container whose networks are all internal
// (moby libnetwork/endpoint.go: external connectivity is programmed only
// `if !n.internal`), silently, and a network that is not internal gives the
// ports back.
func TestPublishSkipsInternalOnly(t *testing.T) {
	spec, err := portmap.ParseSpecs([]string{"127.0.0.1::80"})
	if err != nil {
		t.Fatal(err)
	}
	internal := func(n string) bool { return n == "sealed" || n == "sealed2" }
	for _, tc := range []struct {
		name    string
		nets    []string
		fn      bool
		noHost  bool
		publish bool
	}{
		{"internal only", []string{"sealed"}, true, false, false},
		{"two internal", []string{"sealed", "sealed2"}, true, false, false},
		{"internal only, engine cannot publish", []string{"sealed"}, true, true, false},
		{"internal and ordinary", []string{"sealed", "open"}, true, false, true},
		{"ordinary", []string{"open"}, true, false, true},
		{"no internal func", []string{"sealed"}, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &countHost{Host: portmap.NewVirtual()}
			opt := Options{Ports: h}
			if tc.noHost {
				opt.Ports = nil
			}
			if tc.fn {
				opt.Internal = internal
			}
			m := &Manager{opt: opt}
			var atts []Attachment
			for _, n := range tc.nets {
				atts = append(atts, Attachment{Network: n})
			}
			c := &Container{name: "web", cfg: Config{Networks: atts, Ports: spec}}
			s, err := m.publish(context.Background(), c, netdriver.NewMulti(), atts)
			if err != nil {
				t.Fatal(err)
			}
			if got := h.n.Load() > 0; got != tc.publish || (s != nil) != tc.publish {
				t.Fatalf("published=%v set=%v, want %v", got, s != nil, tc.publish)
			}
			if s != nil {
				s.Close()
			}
		})
	}
}

// internalOnly is false without an Internal func and for no network at all,
// and a reconcile that cannot publish says so and leaves nothing open.
func TestInternalOnlyEdges(t *testing.T) {
	spec, err := portmap.ParseSpecs([]string{"127.0.0.1::80"})
	if err != nil {
		t.Fatal(err)
	}
	sealedOnly := []Attachment{{Network: "sealed"}}
	plain := &Manager{opt: Options{Ports: portmap.NewVirtual()}}
	if plain.internalOnly(sealedOnly) {
		t.Fatal("internal without an Internal func")
	}
	m := &Manager{opt: Options{Internal: func(string) bool { return true }}}
	if m.internalOnly(nil) {
		t.Fatal("no network is not an internal one")
	}
	if !m.internalOnly(sealedOnly) {
		t.Fatal("a sealed network is not internal")
	}
	// Ports are wanted again but the engine cannot publish: an error, and
	// c.ports stays nil.
	m2 := &Manager{opt: Options{Internal: func(n string) bool { return n == "sealed" }}}
	c := &Container{name: "web", state: Running, net: netdriver.NewMulti(), cfg: Config{Ports: spec}}
	if err := m2.reconcilePorts(context.Background(), c, []Attachment{{Network: "open"}}); err == nil || c.ports != nil {
		t.Fatalf("reconcile without a host: %v, ports=%v", err, c.ports)
	}
}

// A running container's ports follow its networks: published on "open", they
// close when "open" is disconnected and only "sealed" is left, and open again
// when an ordinary network is connected (moby re-programs external
// connectivity on every endpoint join and leave).
func TestReconcilePorts(t *testing.T) {
	spec, err := portmap.ParseSpecs([]string{"127.0.0.1::80"})
	if err != nil {
		t.Fatal(err)
	}
	h := &countHost{Host: portmap.NewVirtual()}
	m := &Manager{opt: Options{Ports: h, Internal: func(n string) bool { return n == "sealed" }}}
	c := &Container{name: "web", state: Running, net: netdriver.NewMulti(), cfg: Config{Ports: spec}}
	ctx := context.Background()
	open := []Attachment{{Network: "open"}}
	both := []Attachment{{Network: "open"}, {Network: "sealed"}}
	sealedOnly := []Attachment{{Network: "sealed"}}

	if err := m.reconcilePorts(ctx, c, open); err != nil || c.ports == nil {
		t.Fatalf("open: ports=%v err=%v", c.ports, err)
	}
	if err := m.reconcilePorts(ctx, c, both); err != nil || c.ports == nil {
		t.Fatalf("open+sealed keeps its ports: ports=%v err=%v", c.ports, err)
	}
	before := h.n.Load()
	if err := m.reconcilePorts(ctx, c, sealedOnly); err != nil || c.ports != nil {
		t.Fatalf("sealed only: ports=%v err=%v", c.ports, err)
	}
	if err := m.reconcilePorts(ctx, c, sealedOnly); err != nil || c.ports != nil || h.n.Load() != before {
		t.Fatalf("sealed only again: ports=%v err=%v", c.ports, err)
	}
	if err := m.reconcilePorts(ctx, c, both); err != nil || c.ports == nil {
		t.Fatalf("an ordinary network connected again: ports=%v err=%v", c.ports, err)
	}
	// Dropped to none (the last network was disconnected): Docker publishes
	// nothing there, whether the network it left was internal or not.
	none := []Attachment{{Network: NoNetwork}}
	if err := m.reconcilePorts(ctx, c, none); err != nil || c.ports != nil {
		t.Fatalf("none after open: ports=%v err=%v", c.ports, err)
	}
	if err := m.reconcilePorts(ctx, c, sealedOnly); err != nil || c.ports != nil {
		t.Fatalf("sealed only: ports=%v err=%v", c.ports, err)
	}
	if err := m.reconcilePorts(ctx, c, none); err != nil || c.ports != nil {
		t.Fatalf("none after sealed: ports=%v err=%v", c.ports, err)
	}
	// Not running: nothing to do.
	c2 := &Container{name: "x", state: Created, cfg: Config{Ports: spec}}
	if err := m.reconcilePorts(ctx, c2, open); err != nil || c2.ports != nil {
		t.Fatalf("created container: %v %v", c2.ports, err)
	}
}
