package relaynet

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"

	v1 "github.com/go-wasmcontainers/relay/gen/relayv1"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// Round 2, item 1: a non-finite or negative rate cannot be encoded as JSON
// (NaN, Inf) or means nothing (negative); the description is then "unknown",
// never a state that breaks every `network ls`. EgressEnabled is untouched.
func TestEgressStateRefusesNonFiniteLimits(t *testing.T) {
	for name, v := range map[string]float64{
		"NaN": math.NaN(), "+Inf": math.Inf(1), "-Inf": math.Inf(-1), "negative": -1, "negative tiny": -math.SmallestNonzeroFloat64,
	} {
		for field, l := range map[string]*v1.EgressLimits{
			"connects": {ConnectsPerSecond: v}, "lookups": {LookupsPerSecond: v},
		} {
			d, _ := drv(&v1.EgressState{Policy: "gateway", Limits: l, Allowlist: []string{"pypi.org:443"}})
			s := d.EgressState()
			if s.Policy != netdriver.PolicyUnknown || s.Gateway != nil || len(s.Allowlist) != 0 {
				t.Errorf("%s %s: %+v", name, field, s)
			}
			if _, err := json.Marshal(s); err != nil {
				t.Errorf("%s %s: %v", name, field, err)
			}
			if !d.EgressEnabled() {
				t.Errorf("%s %s: EgressEnabled turned false", name, field)
			}
		}
	}
	// Zero and large finite values are fine.
	d, _ := drv(&v1.EgressState{Policy: "gateway", Limits: &v1.EgressLimits{ConnectsPerSecond: 0, LookupsPerSecond: 1e300}})
	if s := d.EgressState(); s.Policy != netdriver.PolicyGateway {
		t.Errorf("%+v", s)
	}
}

const bidi = "‪‫‬‭‮⁦⁧⁨⁩‎‏"

// Round 2, item 2: what the relay sends is drawn by a UI. No control
// character, no bidi override, bounded.
func TestEgressStateCleansRelayText(t *testing.T) {
	long := strings.Repeat("a", 5000)
	d, _ := drv(&v1.EgressState{Policy: "gateway", Operator: "Lab\x1b[31m IT\n" + bidi + "\x00end" + long,
		Counters: &v1.EgressCounters{LastDeniedHost: "evil" + bidi + ".example.org\r\n", LastDeniedReason: "sni\x07" + bidi + long, LastDeniedUnix: 1700000000}})
	s := d.EgressState()
	if !strings.HasPrefix(s.Gateway.Operator, "Lab[31m ITend") {
		t.Errorf("operator %q", s.Gateway.Operator)
	}
	if n := len([]rune(s.Gateway.Operator)); n != maxOperator {
		t.Errorf("operator %d runes, want %d", n, maxOperator)
	}
	ld := s.Counters.LastDenied
	if ld == nil || ld.Host != "evil.example.org" {
		t.Fatalf("last denied %+v", ld)
	}
	if !strings.HasPrefix(ld.Reason, "sni") || len([]rune(ld.Reason)) != maxDenialReason {
		t.Errorf("reason %d runes %q", len([]rune(ld.Reason)), ld.Reason)
	}
	n, _ := drv(&v1.EgressState{Policy: "none", Reason: "no\x1b egress‮" + long})
	r := n.EgressState().Reason
	if !strings.HasPrefix(r, "no egress") || len([]rune(r)) != maxReason || strings.ContainsAny(r, "\x1b‮") {
		t.Errorf("none reason %d runes %q", len([]rune(r)), r)
	}
	// A reason that is only junk falls back to the standard one.
	n, _ = drv(&v1.EgressState{Policy: "none", Reason: "\x00‮\n"})
	if r := n.EgressState().Reason; r != "class relay without egress" {
		t.Errorf("junk reason %q", r)
	}
	// Invalid UTF-8 is dropped, not passed on.
	g, _ := drv(&v1.EgressState{Policy: "gateway"})
	_ = g
	if got := clean("ok\xff\xfe!", 10); got != "ok!" {
		t.Errorf("invalid utf-8: %q", got)
	}
}

func TestClean(t *testing.T) {
	for _, tc := range []struct {
		in   string
		max  int
		want string
	}{
		{"", 5, ""},
		{"abc", 5, "abc"},
		{"abcde", 5, "abcde"},
		{"abcdef", 5, "abcde"},
		{"é€𝄞xyz", 3, "é€𝄞"},
		{"a\tb\x7fc\u0085d", 10, "abcd"},
		{"a‪b‮c⁦d⁩e", 10, "abcde"},
		{"⁥x⁪y‍z", 10, "⁥xyz"}, // an unassigned neighbour is kept; every format character goes
		{" x ", 10, "x"},       // line and paragraph separators are line breaks
		{"a b-c.d", 10, "a b-c.d"},
	} {
		if got := clean(tc.in, tc.max); got != tc.want {
			t.Errorf("clean(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
	}
}

// Round 2, item 2: the denial's time is a clock reading from another party.
func TestEgressStateClampsDeniedTime(t *testing.T) {
	now0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	defer func(f func() time.Time) { nowFunc = f }(nowFunc)
	nowFunc = func() time.Time { return now0 }
	for name, tc := range map[string]struct {
		unix int64
		ok   bool
	}{
		"zero": {0, false}, "negative": {-1, false}, "before the floor": {minDeniedUnix - 1, false},
		"the floor": {minDeniedUnix, true}, "now": {now0.Unix(), true},
		"a day ahead": {now0.Add(maxSkew).Unix(), true}, "a day and a second ahead": {now0.Add(maxSkew).Unix() + 1, false},
		"max": {math.MaxInt64, false}, "min": {math.MinInt64, false},
	} {
		d, _ := drv(&v1.EgressState{Policy: "gateway", Counters: &v1.EgressCounters{LastDeniedHost: "h.example.org", LastDeniedUnix: tc.unix}})
		ld := d.EgressState().Counters.LastDenied
		if ld == nil {
			t.Fatalf("%s: denial dropped", name)
		}
		if tc.ok != !ld.Time.IsZero() || (tc.ok && ld.Time.Unix() != tc.unix) {
			t.Errorf("%s: time %v", name, ld.Time)
		}
	}
}

// Round 2, item 7: limits and counters are as of join, and say so.
func TestEgressCountersAreLabelledAsOf(t *testing.T) {
	before := time.Now()
	d := New(nil)
	after := time.Now()
	d.src = &fakeSrc{w: &v1.Welcome{Egress: &v1.EgressState{Policy: "gateway", Counters: &v1.EgressCounters{Allowed: 1}}}, done: make(chan struct{})}
	at := d.EgressState().Counters.AsOf
	if at.Before(before) || at.After(after) {
		t.Fatalf("as of %v, joined between %v and %v", at, before, after)
	}
	// A driver built without New has no join time: the label says so (zero).
	e, _ := drv(&v1.EgressState{Policy: "gateway", Counters: &v1.EgressCounters{}})
	if !e.EgressState().Counters.AsOf.IsZero() {
		t.Error("an unknown join time was invented")
	}
}

// Round 2, item 7: NotifyEgress never fires because relay v0.4.0 has no
// message that changes egress after the Welcome. If a later relay adds one
// (any new message about egress, a policy, an update or a refresh of it), the
// no-op NotifyEgress becomes unsafe on an internal network: this test fails
// so that whoever bumps relay has to read it.
func TestRelayHasNoEgressRefreshMessage(t *testing.T) {
	known := map[string]bool{"EgressState": true, "EgressLimits": true, "EgressCounters": true, "RefreshSessionRequest": true, "RefreshSessionResponse": true}
	msgs := v1.File_wasmcontainers_relay_v1_relay_proto.Messages()
	if msgs.Len() < 50 {
		t.Fatalf("only %d messages: descriptor not read", msgs.Len())
	}
	for i := 0; i < msgs.Len(); i++ {
		var m protoreflect.MessageDescriptor = msgs.Get(i)
		n := string(m.Name())
		for _, w := range []string{"egress", "policy", "refresh", "update", "changed", "notify"} {
			if strings.Contains(strings.ToLower(n), w) && !known[n] && n != "UpdatePortRequest" && n != "FlowMapUpdate" {
				t.Errorf("relay message %q: does it change egress after the Welcome? Then NotifyEgress must fire (network/sealed.go relies on it)", n)
			}
		}
	}
	// And the Welcome's egress block is the only one: no other message carries it.
	for i := 0; i < msgs.Len(); i++ {
		m := msgs.Get(i)
		for j := 0; j < m.Fields().Len(); j++ {
			f := m.Fields().Get(j)
			if f.Message() != nil && f.Message().Name() == "EgressState" && m.Name() != "Welcome" {
				t.Errorf("%s.%s carries an EgressState: egress can change after the Welcome", m.Name(), f.Name())
			}
		}
	}
}
