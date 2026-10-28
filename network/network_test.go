package network

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/state"
)

func TestManager(t *testing.T) {
	st := &state.Mem{}
	m, rep, err := New(Options{Store: st})
	if err != nil || len(rep) != 0 {
		t.Fatal(err, rep)
	}
	if ls := m.List(); len(ls) != 2 || ls[0].Name != Bridge || ls[1].Name != None {
		t.Fatalf("%+v", ls)
	}
	n, err := m.Create("lab", map[string]string{"k": "v"})
	if err != nil || n.Subnet != "10.89.0.0/16" || n.Driver != "bridge" {
		t.Fatal(n, err)
	}
	n2, _ := m.Create("lab2", nil)
	if n2.Subnet != "10.90.0.0/16" {
		t.Fatal(n2.Subnet)
	}
	for _, bad := range []string{"lab", "host", "", "-x", "a b", strings.Repeat("a", 65)} {
		if _, err := m.Create(bad, nil); err == nil {
			t.Errorf("created %q", bad)
		}
	}
	if got, err := m.Get(n.ID[:6]); err != nil || got.Name != "lab" {
		t.Fatal(got, err)
	}
	// Attach: the default network, a created one, none (no endpoint).
	ctx := context.Background()
	ep, err := m.Attach(ctx, "", "web", nil)
	if err != nil || !netdriver.DefaultPrefix.Contains(ep.Addr()) {
		t.Fatal(ep, err)
	}
	ep, err = m.Attach(ctx, "lab", "web", []string{"www"})
	if err != nil || ep.Addr().String() != "10.89.0.2" {
		t.Fatal(ep, err)
	}
	if got, err := ep.Resolve(ctx, "www"); err != nil || len(got) != 1 || got[0] != ep.Addr() {
		t.Fatal(got, err)
	}
	if ep, err := m.Attach(ctx, None, "web", nil); err != nil || ep != nil {
		t.Fatal(ep, err)
	}
	if _, err := m.Attach(ctx, "nowhere", "web", nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	// Aliases: user-defined networks only, as Docker.
	for _, net := range []string{"", Bridge, None} {
		if _, err := m.Attach(ctx, net, "web2", []string{"www"}); !errors.Is(err, ErrInvalid) {
			t.Errorf("aliases on %q: %v", net, err)
		}
	}
	// Removal: refused while used, and for builtins.
	if err := m.Remove("lab", func(string) bool { return true }); !errors.Is(err, ErrInUse) {
		t.Fatal(err)
	}
	if err := m.Remove(Bridge, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := m.Remove("lab2", nil); err != nil {
		t.Fatal(err)
	}
	// Prune: unused created networks whose labels match.
	m.Create("keep", map[string]string{"app": "a"})
	m.Create("drop", map[string]string{"app": "b"})
	gone := m.Prune(func(n string) bool { return n == "lab" }, func(l map[string]string) bool { return l["app"] != "a" })
	if len(gone) != 1 || gone[0] != "drop" {
		t.Fatalf("pruned %v, want [drop]", gone)
	}
	if _, err := m.Get("keep"); err != nil {
		t.Fatal(err)
	}
	m.Close()

	// A restart keeps "lab", not "lab2"; a damaged record is dropped and
	// reported.
	st.Put("network/"+keyName("broken"), []byte("{not json"))
	m, rep, err = New(Options{Store: st})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if len(rep) != 1 || !strings.Contains(rep[0], "dropped") {
		t.Fatalf("repairs %v", rep)
	}
	if got, err := m.Get("lab"); err != nil || got.ID != n.ID || got.Labels["k"] != "v" {
		t.Fatal(got, err)
	}
	if _, err := m.Get("lab2"); err == nil {
		t.Fatal("removed network came back")
	}
}

func TestHostDrivers(t *testing.T) {
	d, _ := netdriver.NewLocal(netdriver.DefaultPrefix)
	if _, _, err := New(Options{Drivers: map[string]netdriver.NetworkDriver{Bridge: d}}); err == nil {
		t.Fatal("a driver took the name bridge")
	}
	m, _, err := New(Options{Drivers: map[string]netdriver.NetworkDriver{"class": d}, DriverName: "relay"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	n, err := m.Get("class")
	if err != nil || n.Driver != "relay" || !n.Builtin {
		t.Fatal(n, err)
	}
	if err := m.Remove("class", nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := m.Create("class", nil); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	// At run time: add, attach, remove (the driver comes back, open).
	d2, _ := netdriver.NewLocal(netdriver.DefaultPrefix)
	if _, err := m.AddDriver("class", "relay", d2); !errors.Is(err, ErrExists) {
		t.Fatal(err)
	}
	if n, err := m.AddDriver("joined", "relay", d2); err != nil || n.Driver != "relay" || !n.Builtin {
		t.Fatal(n, err)
	}
	if ep, err := m.Attach(context.Background(), "joined", "web", nil); err != nil || ep == nil {
		t.Fatal(ep, err)
	}
	if err := m.Remove("joined", nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("Remove took a run-time driver:", err)
	}
	if _, err := m.RemoveDriver("class"); !errors.Is(err, ErrNotFound) {
		t.Fatal("RemoveDriver took a start-time driver:", err)
	}
	if got, err := m.RemoveDriver("joined"); err != nil || got != d2 {
		t.Fatal(got, err)
	}
	if _, err := m.Attach(context.Background(), "joined", "web", nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	d2.Close()
}
