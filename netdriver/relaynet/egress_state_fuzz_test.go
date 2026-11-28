package relaynet

import (
	"encoding/json"
	"math"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	v1 "github.com/go-wasmcontainers/relay/gen/relayv1"
	"google.golang.org/protobuf/proto"

	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/netdriver"
)

// egressSeeds are Welcomes as a relay could send them, encoded; the committed
// corpus of FuzzEgressState is made of them.
func egressSeeds() [][]byte {
	long := string(make([]byte, 300))
	ws := []*v1.Welcome{
		{},
		{Egress: &v1.EgressState{}},
		{Egress: &v1.EgressState{Policy: "none"}},
		{Egress: &v1.EgressState{Policy: "none", Reason: "no\x1b egress‮" + long}},
		{Egress: &v1.EgressState{Policy: "tunnel"}},
		{Egress: &v1.EgressState{Policy: "gateway", Operator: "Lab IT", Logs: true,
			Allowlist: []string{"pypi.org:443", "*.x.org:443", "1.2.3.4:443", "h:80", "Pypi.org:443"},
			Limits:    &v1.EgressLimits{ConnectsPerSecond: 5, LookupsPerSecond: 10},
			Counters: &v1.EgressCounters{Allowed: 3, LastDeniedHost: "evil⁦.example.org\r\n",
				LastDeniedReason: "sni\x07", LastDeniedUnix: 1700000000}}},
		{Egress: &v1.EgressState{Policy: "gateway", Limits: &v1.EgressLimits{ConnectsPerSecond: math.NaN()}}},
		{Egress: &v1.EgressState{Policy: "gateway", Limits: &v1.EgressLimits{LookupsPerSecond: math.Inf(1)}}},
		{Egress: &v1.EgressState{Policy: "gateway", Limits: &v1.EgressLimits{ConnectsPerSecond: -1}}},
		{Egress: &v1.EgressState{Policy: "gateway", Counters: &v1.EgressCounters{LastDeniedHost: "h.example.org", LastDeniedUnix: math.MaxInt64}}},
		// (Invalid UTF-8 is no seed: proto3 refuses it in a string field on
		// both ends; clean drops it anyway, see TestEgressStateCleansRelayText.)
		{Egress: &v1.EgressState{Policy: "gateway", Operator: "⁧" + long + " "}},
	}
	out := make([][]byte, 0, len(ws))
	for _, w := range ws {
		b, err := proto.Marshal(w)
		if err != nil {
			panic(err)
		}
		out = append(out, b)
	}
	return out
}

// shown reports whether s is text a UI may draw: valid UTF-8, no control or
// format character (every bidi override and isolate is one), no line or
// paragraph separator, at most max runes.
func shown(s string, max int) bool {
	if !utf8.ValidString(s) || utf8.RuneCountInString(s) > max {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			return false
		}
	}
	return true
}

// FuzzEgressState: whatever Welcome the relay sends, the description is one
// of the known policies, encodes as JSON (no NaN, no Inf), carries only
// drawable bounded text and a credible denial time, shows only 443, and
// never panics.
func FuzzEgressState(f *testing.F) {
	for _, s := range egressSeeds() {
		f.Add(s)
	}
	now0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)
	nowFunc = func() time.Time { return now0 }
	f.Cleanup(func() { nowFunc = time.Now })
	f.Fuzz(func(t *testing.T, in []byte) {
		var w v1.Welcome
		if proto.Unmarshal(in, &w) != nil {
			return
		}
		d := &Driver{src: &fakeSrc{w: &w, done: make(chan struct{})}}
		s := d.EgressState()
		if _, err := json.Marshal(s); err != nil {
			t.Fatalf("does not encode: %v (%+v)", err, s)
		}
		if !shown(s.Reason, 256) {
			t.Fatalf("reason %q", s.Reason)
		}
		switch s.Policy {
		case netdriver.PolicyNone, netdriver.PolicyUnknown:
			if s.Gateway != nil || len(s.Allowlist) != 0 || s.Counters.LastDenied != nil {
				t.Fatalf("%s with a gateway's details: %+v", s.Policy, s)
			}
		case netdriver.PolicyGateway:
			if s.Gateway == nil || !shown(s.Gateway.Operator, maxOperator) {
				t.Fatalf("gateway %+v", s.Gateway)
			}
		default:
			t.Fatalf("policy %q", s.Policy)
		}
		if len(s.Allowlist) > maxAllowlist {
			t.Fatalf("%d allowlist entries", len(s.Allowlist))
		}
		for _, a := range s.Allowlist {
			if a.Port != netdriver.EgressPort {
				t.Fatalf("port %d", a.Port)
			}
		}
		if ld := s.Counters.LastDenied; ld != nil {
			if ld.Host == "" || !shown(ld.Host, maxDenialHost) || !shown(ld.Reason, maxDenialReason) {
				t.Fatalf("denial %+v", ld)
			}
			if !ld.Time.IsZero() && (ld.Time.Unix() < minDeniedUnix || ld.Time.After(now0.Add(maxSkew))) {
				t.Fatalf("denial time %v", ld.Time)
			}
		}
	})
}

// TestWriteEgressCorpus regenerates the committed corpora of FuzzEgressState
// and FuzzOrigin (WRITE_FUZZ_CORPUS=1).
func TestWriteEgressCorpus(t *testing.T) {
	seeds := make([]any, 0, 16)
	for _, s := range egressSeeds() {
		seeds = append(seeds, s)
	}
	testutil.WriteCorpus(t, "FuzzEgressState", seeds...)
	testutil.WriteCorpus(t, "FuzzOrigin", "https://relay.example:8443", "https://u:pw@relay.example/x?token=abc#frag",
		"http://[::1]:80", "https://relay.example:99999", "wss://x", "https://", "", "https://a b", "https://%00",
		"https:///x", "https://@", "https://relay.example‮/x", "HTTPS://Relay.Example:443")
}
