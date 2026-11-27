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
			s, err := m.publish(context.Background(), c, netdriver.NewMulti())
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
