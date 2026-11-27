package container

import (
	"context"
	"net/netip"
	"testing"

	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/portmap"
)

// network connect and disconnect on a running container re-decide its ports,
// and a refusal leaves the container as it was.
func TestConnectDisconnectReconcile(t *testing.T) {
	spec, err := portmap.ParseSpecs([]string{"127.0.0.1::80"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	drv := map[string]*netdriver.Local{}
	for n, p := range map[string]string{"open": "10.77.0.0/24", "sealed": "10.77.1.0/24"} {
		d, err := netdriver.NewLocal(netip.MustParsePrefix(p))
		if err != nil {
			t.Fatal(err)
		}
		drv[n] = d
	}
	// No host to publish on: reopening the ports is refused.
	m := &Manager{byID: map[string]*Container{}, byName: map[string]*Container{}, opt: Options{
		Internal: func(n string) bool { return n == "sealed" },
		Network: func(ctx context.Context, n, name string, al []string) (netdriver.Endpoint, error) {
			return drv[n].Attach(ctx, name)
		},
	}}
	mn := netdriver.NewMulti()
	ep, err := drv["sealed"].Attach(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if err := mn.Add(ctx, "sealed", ep); err != nil {
		t.Fatal(err)
	}
	c := &Container{id: "abcdef0123456789", name: "web", state: Running, net: mn,
		cfg: Config{Networks: []Attachment{{Network: "sealed"}}, Ports: spec}}
	m.byID[c.id] = c
	m.byName[c.name] = c

	if err := m.Connect(ctx, "web", Attachment{Network: "open"}); err == nil {
		t.Fatal("connect that must publish ports on an engine that cannot")
	}
	if len(c.cfg.Networks) != 1 || len(mn.Endpoints()) != 1 {
		t.Fatalf("a refused connect changed the container: %v, %d endpoints", c.cfg.Networks, len(mn.Endpoints()))
	}

	// Both networks, ports closed (as when connected while sealed only):
	// leaving the sealed one asks for ports the engine cannot open.
	ep2, err := drv["open"].Attach(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if err := mn.Add(ctx, "open", ep2); err != nil {
		t.Fatal(err)
	}
	c.cfg.Networks = []Attachment{{Network: "sealed"}, {Network: "open"}}
	if err := m.Disconnect("web", "sealed"); err == nil {
		t.Fatal("disconnect that must publish ports on an engine that cannot")
	}
	if len(c.cfg.Networks) != 2 {
		t.Fatalf("a refused disconnect changed the networks: %v", c.cfg.Networks)
	}
}
