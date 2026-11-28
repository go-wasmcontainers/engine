package network

import (
	"context"
	"errors"
	"testing"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// A driver that CAN leave but describes itself as anything other than a
// gateway is "unknown", never believed: "internal" and "unknown" and an empty
// policy would otherwise be shown as the description says (round 2, item 3).
func TestNetworkEgressEnabledButNotDescribedAsGateway(t *testing.T) {
	m, _, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	for i, pol := range []string{netdriver.PolicyInternal, netdriver.PolicyUnknown, "", "tunnel", netdriver.PolicyNone} {
		name := "n" + string(rune('a'+i))
		st := netdriver.NetworkEgress{Policy: pol, Reason: "claimed " + pol, Allowlist: []netdriver.EgressDest{{Host: "x.example.org", Port: 443}}}
		if _, err := m.AddDriver(name, "remote", reporter{localDriver(t), st, true}); err != nil {
			t.Fatal(err)
		}
		n, _ := m.Get(name)
		if n.Egress.Policy != netdriver.PolicyUnknown || n.Egress.Gateway != nil || len(n.Egress.Allowlist) != 0 {
			t.Errorf("described as %q: %+v", pol, n.Egress)
		}
	}
}

// r2relay behaves as the relay driver does once its Welcome says "none":
// EgressEnabled false, endpoints Remote, and the driver itself Remote. It
// records what reaches "the relay".
type r2relay struct {
	l        *netdriver.Local
	attached []string
	local    bool // Remote() reports false while set
}

func (r *r2relay) EgressEnabled() bool { return false }
func (r *r2relay) Remote() bool        { return !r.local }
func (r *r2relay) Close() error        { return nil }
func (r *r2relay) Attach(ctx context.Context, c string) (netdriver.Endpoint, error) {
	r.attached = append(r.attached, c)
	return r.l.Attach(ctx, c)
}

// Round 2, item 5: a Remote driver is somebody else's machine. It is refused
// on an internal network when added, so no container name ever reaches it.
func TestInternalRefusesARemoteDriverEvenWithoutEgress(t *testing.T) {
	m, _, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	d := &r2relay{l: localDriver(t)}
	_, err = m.AddDriverWith("class", "relay", d, true)
	if !errors.Is(err, ErrEgress) {
		t.Fatalf("admitted: %v", err)
	}
	if len(d.attached) != 0 {
		t.Fatalf("the relay saw %v", d.attached)
	}
	// A driver that was local when added and is remote at attach time is
	// refused at attach, before it is asked.
	d.local = true
	if _, err := m.AddDriverWith("late", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	d.local = false
	if _, err := m.Attach(context.Background(), "late", "secret-name", nil); !errors.Is(err, ErrEgress) {
		t.Fatalf("attach: %v", err)
	}
	if len(d.attached) != 0 {
		t.Fatalf("the relay saw %v", d.attached)
	}
	// Not internal: the same driver is fine (it is just a remote network).
	if _, err := m.AddDriver("plain", "relay", d); err != nil {
		t.Fatal(err)
	}
}
