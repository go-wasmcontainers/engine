package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/api"
)

// TestNetworkInternal: the API accepts Internal (compose sends it for
// `internal: true`), network inspect and ls report it, an ordinary network
// reports false, and options are still refused.
func TestNetworkInternal(t *testing.T) {
	h := netDaemon(t, shortDir(t))
	c, err := api.NewClient(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	r, err := c.NetworkCreate(ctx, api.NetworkCreateRequest{Name: "sealed", Internal: true})
	if err != nil {
		t.Fatalf("create internal: %v", err)
	}
	if _, err := c.NetworkCreate(ctx, api.NetworkCreateRequest{Name: "open"}); err != nil {
		t.Fatal(err)
	}
	raw, err := c.NetworkInspect(ctx, "sealed")
	if err != nil {
		t.Fatal(err)
	}
	var got api.Network
	if err := json.Unmarshal(raw, &got); err != nil || got.ID != r.ID || !got.Internal {
		t.Fatalf("inspect sealed: %s (%v)", raw, err)
	}
	raw, _ = c.NetworkInspect(ctx, "open")
	got = api.Network{}
	if err := json.Unmarshal(raw, &got); err != nil || got.Internal {
		t.Fatalf("inspect open: %s (%v)", raw, err)
	}
	list, err := c.Networks(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range list {
		if n.Internal != (n.Name == "sealed") {
			t.Errorf("ls: %s internal=%v", n.Name, n.Internal)
		}
	}
	if _, err := c.NetworkCreate(ctx, api.NetworkCreateRequest{Name: "opts", Options: map[string]string{"x": "y"}}); err == nil {
		t.Error("options accepted")
	}

	// Egress (D5): the default bridge and a created network have none; an
	// internal one says it is internal. Nothing here advertises a gateway.
	for name, want := range map[string]string{"bridge": "none", "none": "none", "open": "none", "sealed": "internal"} {
		raw, err := c.NetworkInspect(ctx, name)
		if err != nil {
			t.Fatal(name, err)
		}
		var n api.Network
		if err := json.Unmarshal(raw, &n); err != nil || n.Egress.Policy != want || n.Egress.Gateway != nil || len(n.Egress.Allowlist) != 0 {
			t.Errorf("%s: egress %+v (%v), want policy %s\n%s", name, n.Egress, err, want, raw)
		}
	}
	for _, n := range list {
		if n.Egress.Policy == "" || n.Egress.Policy == "gateway" {
			t.Errorf("ls: %s egress %+v", n.Name, n.Egress)
		}
	}

	// The CLI flag.
	id := must(t, wctr(t, h, "network", "create", "--internal", "viacli"), 0).stdout
	if raw := must(t, wctr(t, h, "network", "inspect", strings.TrimSpace(id)), 0).stdout; !strings.Contains(raw, `"Internal": true`) {
		t.Fatalf("inspect viacli:\n%s", raw)
	}
}
