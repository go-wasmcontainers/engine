package network

import (
	"context"
	"testing"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// reporter is a relay-like driver that describes its egress.
type reporter struct {
	*netdriver.Local
	st netdriver.NetworkEgress
	on bool
}

func (r reporter) EgressEnabled() bool                  { return r.on }
func (r reporter) EgressState() netdriver.NetworkEgress { return r.st }

var gw = netdriver.NetworkEgress{
	Policy:    netdriver.PolicyGateway,
	Reason:    "class relay",
	Gateway:   &netdriver.EgressGateway{Kind: "relay", State: "up", Operator: "Lab"},
	Allowlist: []netdriver.EgressDest{{Host: "pypi.org", Port: 443}},
}

// D5 and section 11: bridge, none and a user-created network have no egress;
// an internal one says so; a class relay with egress says gateway.
func TestNetworkEgressReport(t *testing.T) {
	m, _, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.CreateWith("sealed", nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Create("mine", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddDriver("class", "remote", reporter{localDriver(t), gw, true}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddDriver("quiet", "remote", reporter{localDriver(t), netdriver.NetworkEgress{Policy: netdriver.PolicyNone, Reason: "class relay without egress"}, false}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.AddDriver("mute", "remote", egressDriver{localDriver(t), true}); err != nil {
		t.Fatal(err)
	}
	type want struct{ policy, reason string }
	for name, w := range map[string]want{
		Bridge:   {netdriver.PolicyNone, "engine-internal network"},
		None:     {netdriver.PolicyNone, "engine-internal network"},
		"mine":   {netdriver.PolicyNone, "engine-internal network"},
		"sealed": {netdriver.PolicyInternal, "internal: true"},
		"class":  {netdriver.PolicyGateway, "class relay"},
		"quiet":  {netdriver.PolicyNone, "class relay without egress"},
		// Declares egress but describes nothing: it may leave, and says so.
		"mute": {netdriver.PolicyGateway, "the driver declares egress"},
	} {
		n, err := m.Get(name)
		if err != nil {
			t.Fatal(name, err)
		}
		if n.Egress.Policy != w.policy || n.Egress.Reason != w.reason {
			t.Errorf("%s: %q / %q, want %q / %q", name, n.Egress.Policy, n.Egress.Reason, w.policy, w.reason)
		}
		if (n.Egress.Gateway != nil) != (name == "class") {
			t.Errorf("%s: gateway %+v", name, n.Egress.Gateway)
		}
		if name != "class" && len(n.Egress.Allowlist) != 0 {
			t.Errorf("%s advertises an allowlist: %+v", name, n.Egress.Allowlist)
		}
	}
	if c, _ := m.Get("class"); len(c.Egress.Allowlist) != 1 || c.Egress.Allowlist[0].Port != 443 {
		t.Errorf("class allowlist %+v", c.Egress.Allowlist)
	}
	for _, n := range m.List() {
		g, _ := m.Get(n.Name)
		if n.Egress.Policy != g.Egress.Policy {
			t.Errorf("List and Get disagree on %s", n.Name)
		}
	}
}

// An unknown driver (no Confined, no EgressDriver) is not reported as
// having no egress.
func TestNetworkEgressUnknownDriver(t *testing.T) {
	m, _, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if _, err := m.AddDriver("odd", "remote", opaque{localDriver(t)}); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.Get("odd"); n.Egress.Policy != netdriver.PolicyUnknown {
		t.Fatalf("%+v", n.Egress)
	}
}

// opaque hides *Local's identity: it is a NetworkDriver and nothing more.
type opaque struct{ d netdriver.NetworkDriver }

func (o opaque) Attach(ctx context.Context, c string) (netdriver.Endpoint, error) {
	return o.d.Attach(ctx, c)
}
func (o opaque) Close() error { return o.d.Close() }

// A driver that says "gateway" is reported live: EgressState is asked at every
// read, not cached at AddDriver.
func TestNetworkEgressIsLive(t *testing.T) {
	m, _, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	r := &flip{Local: localDriver(t)}
	if _, err := m.AddDriver("class", "remote", r); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.Get("class"); n.Egress.Policy != netdriver.PolicyNone {
		t.Fatalf("%+v", n.Egress)
	}
	r.on = true
	if n, _ := m.Get("class"); n.Egress.Policy != netdriver.PolicyGateway {
		t.Fatalf("%+v", n.Egress)
	}
}

type flip struct {
	*netdriver.Local
	on bool
}

func (f *flip) EgressEnabled() bool { return f.on }
func (f *flip) EgressState() netdriver.NetworkEgress {
	if f.on {
		return gw
	}
	return netdriver.NetworkEgress{Policy: netdriver.PolicyNone}
}

// EgressEnabled is the authority; a description that disagrees with it is
// never believed in the direction that would hide egress.
func TestNetworkEgressDisagreement(t *testing.T) {
	m, _, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	// Says gateway, declares none: nothing leaves.
	if _, err := m.AddDriver("a", "remote", reporter{localDriver(t), gw, false}); err != nil {
		t.Fatal(err)
	}
	// Declares egress, describes none: it may leave, and the report says unknown.
	if _, err := m.AddDriver("b", "remote", reporter{localDriver(t), netdriver.NetworkEgress{Policy: netdriver.PolicyNone}, true}); err != nil {
		t.Fatal(err)
	}
	if n, _ := m.Get("a"); n.Egress.Policy != netdriver.PolicyNone || n.Egress.Gateway != nil || len(n.Egress.Allowlist) != 0 {
		t.Errorf("a: %+v", n.Egress)
	}
	if n, _ := m.Get("b"); n.Egress.Policy != netdriver.PolicyUnknown {
		t.Errorf("b: %+v", n.Egress)
	}
}

// The reported allowlist is a copy: a caller cannot edit the driver's.
func TestNetworkEgressIsACopy(t *testing.T) {
	m, _, err := New(Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	st := gw
	st.Allowlist = []netdriver.EgressDest{{Host: "pypi.org", Port: 443}}
	if _, err := m.AddDriver("class", "remote", reporter{localDriver(t), st, true}); err != nil {
		t.Fatal(err)
	}
	n, _ := m.Get("class")
	n.Egress.Allowlist[0].Host = "evil.example.org"
	if st.Allowlist[0].Host != "pypi.org" {
		t.Fatal("aliased")
	}
}
