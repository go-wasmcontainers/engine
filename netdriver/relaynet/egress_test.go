package relaynet

import (
	"testing"
	"time"

	v1 "github.com/go-wasmcontainers/relay/gen/relayv1"

	"github.com/go-wasmcontainers/engine/netdriver"
)

type fakeSrc struct {
	w    *v1.Welcome
	done chan struct{}
}

func (f *fakeSrc) Welcome() *v1.Welcome  { return f.w }
func (f *fakeSrc) Done() <-chan struct{} { return f.done }

func drv(eg *v1.EgressState) (*Driver, *fakeSrc) {
	f := &fakeSrc{w: &v1.Welcome{Egress: eg}, done: make(chan struct{})}
	return &Driver{src: f}, f
}

// The relay driver is an egress driver, and a reporter, and a notifier.
var (
	_ netdriver.EgressDriver   = (*Driver)(nil)
	_ netdriver.EgressReporter = (*Driver)(nil)
	_ netdriver.EgressNotifier = (*Driver)(nil)
)

func TestEgressEnabled(t *testing.T) {
	for _, tc := range []struct {
		name string
		eg   *v1.EgressState
		want bool
	}{
		{"older relay, no state", nil, false},
		{"none", &v1.EgressState{Policy: "none"}, false},
		{"gateway", &v1.EgressState{Policy: "gateway"}, true},
		// Fail closed: anything but a plain "none" may leave.
		{"unknown policy", &v1.EgressState{Policy: "tunnel"}, true},
		{"empty policy", &v1.EgressState{}, true},
		{"case matters", &v1.EgressState{Policy: "None"}, true},
	} {
		d, _ := drv(tc.eg)
		if got := d.EgressEnabled(); got != tc.want {
			t.Errorf("%s: EgressEnabled = %v, want %v", tc.name, got, tc.want)
		}
		if got := netdriver.Confined(d); got != !tc.want {
			t.Errorf("%s: Confined = %v, want %v", tc.name, got, !tc.want)
		}
	}
	// A driver with no welcome at all is not confined: unknown fails closed.
	d := &Driver{src: &fakeSrc{done: make(chan struct{})}}
	if !d.EgressEnabled() || netdriver.Confined(d) {
		t.Error("nil Welcome")
	}
	if s := d.EgressState(); s.Policy != netdriver.PolicyUnknown {
		t.Errorf("nil Welcome reports %q", s.Policy)
	}
}

func TestEgressStateGateway(t *testing.T) {
	d, f := drv(&v1.EgressState{
		Policy: "gateway", Operator: "Lab IT", Logs: true,
		Allowlist: []string{"pypi.org:443", "api.example.org:443"},
		Limits:    &v1.EgressLimits{StreamsPerSession: 16, ConnectsPerSecond: 2, ConnectBurst: 20, IdleSeconds: 300, LifetimeSeconds: 3600},
		Counters: &v1.EgressCounters{Allowed: 3, DeniedTlsName: 1, DroppedUdp: 7, BytesUp: 10, BytesDown: 20, ActiveStreams: 2,
			LastDeniedHost: "evil.example.org", LastDeniedReason: "sni", LastDeniedUnix: 1700000000},
	})
	d.SetOrigin("https://u:pw@relay.example:8443/v1/data?token=SECRET")
	s := d.EgressState()
	if s.Policy != netdriver.PolicyGateway || s.Reason != "class relay" {
		t.Fatalf("policy %q reason %q", s.Policy, s.Reason)
	}
	g := s.Gateway
	if g == nil || g.Kind != "relay" || g.Operator != "Lab IT" || !g.Logs || g.State != "up" || g.Origin != "https://relay.example:8443" {
		t.Fatalf("gateway %+v", g)
	}
	want := []netdriver.EgressDest{{Host: "pypi.org", Port: 443}, {Host: "api.example.org", Port: 443}}
	if len(s.Allowlist) != 2 || s.Allowlist[0] != want[0] || s.Allowlist[1] != want[1] {
		t.Fatalf("allowlist %+v", s.Allowlist)
	}
	if s.Limits.StreamsPerSession != 16 || s.Limits.ConnectBurst != 20 || s.Limits.LifetimeSeconds != 3600 {
		t.Fatalf("limits %+v", s.Limits)
	}
	c := s.Counters
	if c.Allowed != 3 || c.DeniedTLSName != 1 || c.DroppedUDP != 7 || c.BytesUp != 10 || c.BytesDown != 20 || c.ActiveStreams != 2 {
		t.Fatalf("counters %+v", c)
	}
	if c.LastDenied == nil || c.LastDenied.Host != "evil.example.org" || c.LastDenied.Reason != "sni" || !c.LastDenied.Time.Equal(time.Unix(1700000000, 0)) {
		t.Fatalf("last denied %+v", c.LastDenied)
	}
	close(f.done)
	if g := d.EgressState().Gateway; g == nil || g.State != "down" {
		t.Fatalf("after the session ended: %+v", g)
	}
	if !d.EgressEnabled() {
		t.Error("a dead session must not turn EgressEnabled false")
	}
}

func TestEgressStateNoLastDenied(t *testing.T) {
	d, _ := drv(&v1.EgressState{Policy: "gateway", Counters: &v1.EgressCounters{}})
	if s := d.EgressState(); s.Counters.LastDenied != nil || s.Gateway.Origin != "" {
		t.Fatalf("%+v", s)
	}
}

func TestEgressStateNone(t *testing.T) {
	for name, eg := range map[string]*v1.EgressState{
		"no state": nil,
		"none":     {Policy: "none", Reason: "class has no allowlist", Allowlist: []string{"x.example.org:443"}},
	} {
		d, _ := drv(eg)
		s := d.EgressState()
		if s.Policy != netdriver.PolicyNone || s.Gateway != nil || len(s.Allowlist) != 0 {
			t.Errorf("%s: %+v", name, s)
		}
		if s.Reason == "" {
			t.Errorf("%s: no reason", name)
		}
	}
	d, _ := drv(&v1.EgressState{Policy: "none", Reason: "class has no allowlist"})
	if r := d.EgressState().Reason; r != "class has no allowlist" {
		t.Errorf("reason %q", r)
	}
}

func TestEgressStateUnknownPolicy(t *testing.T) {
	d, _ := drv(&v1.EgressState{Policy: "tunnel", Allowlist: []string{"pypi.org:443"}})
	s := d.EgressState()
	if s.Policy != netdriver.PolicyUnknown || s.Gateway != nil || len(s.Allowlist) != 0 {
		t.Fatalf("%+v", s)
	}
}

// D3 and D5: only exact host names on 443 are shown; whatever else the relay
// listed is not advertised.
func TestEgressAllowlistIs443ExactHostsOnly(t *testing.T) {
	d, _ := drv(&v1.EgressState{Policy: "gateway", Allowlist: []string{
		"ok.example.org:443",
		"plain.example.org:80", "alt.example.org:8443", "wild.*.org:443", "*.example.org:443",
		"93.184.216.34:443", "[::1]:443", "single:443", "Upper.Example.org:443", "no-port.example.org", "", ":443",
		"ok.example.org:443", // a duplicate is shown once
	}})
	got := d.EgressState().Allowlist
	if len(got) != 1 || got[0] != (netdriver.EgressDest{Host: "ok.example.org", Port: 443}) {
		t.Fatalf("%+v", got)
	}
}

func TestEgressAllowlistBounded(t *testing.T) {
	var l []string
	for i := 0; i < 5000; i++ {
		l = append(l, "h"+itoa(i)+".example.org:443")
	}
	d, _ := drv(&v1.EgressState{Policy: "gateway", Allowlist: l})
	if n := len(d.EgressState().Allowlist); n != maxAllowlist {
		t.Fatalf("%d entries shown, want %d", n, maxAllowlist)
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for ; i > 0; i /= 10 {
		b = append([]byte{byte('0' + i%10)}, b...)
	}
	return string(b)
}

// The Welcome is the only thing that says egress is on, and it never changes
// in v0.4.0: so the notifier has nothing to announce and stop is harmless.
func TestNotifyEgressNeverFires(t *testing.T) {
	d, _ := drv(&v1.EgressState{Policy: "none"})
	fired := false
	stop := d.NotifyEgress(func() { fired = true })
	stop()
	stop()
	if fired {
		t.Fatal("fired")
	}
}

func TestOrigin(t *testing.T) {
	for in, want := range map[string]string{
		"https://relay.example":                       "https://relay.example",
		"https://relay.example:8443/":                 "https://relay.example:8443",
		"https://u:pw@relay.example:8443/x?token=abc": "https://relay.example:8443",
		"http://127.0.0.1:9000":                       "http://127.0.0.1:9000",
		"HTTPS://Relay.Example":                       "https://relay.example",
		"wss://relay.example/v1/data":                 "",
		"relay.example":                               "",
		"https://":                                    "",
		"":                                            "",
		"https://relay.example:99999":                 "",
		"https://rel ay.example":                      "",
		"https://rel_ay.example":                      "",
		"https://relay.exam~ple":                      "",
		"https://relay.example$:443":                  "",
		"https://[::1]:8443":                          "https://[::1]:8443",
	} {
		if got := originOf(in); got != want {
			t.Errorf("originOf(%q) = %q, want %q", in, got, want)
		}
	}
}
