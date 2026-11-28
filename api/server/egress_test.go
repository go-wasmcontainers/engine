package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver"
)

func TestEgressObject(t *testing.T) {
	when := time.Unix(1700000000, 0).UTC()
	o := egressObject(netdriver.NetworkEgress{
		Policy: netdriver.PolicyGateway, Reason: "class relay",
		Gateway:   &netdriver.EgressGateway{Kind: "relay", Origin: "https://relay.example:8443", Operator: "Lab", State: "up", Logs: true},
		Allowlist: []netdriver.EgressDest{{Host: "pypi.org", Port: 443}},
		Limits:    netdriver.EgressLimits{StreamsPerSession: 16},
		Counters:  netdriver.EgressCounters{Allowed: 2, DroppedUDP: 5, LastDenied: &netdriver.EgressDenial{Host: "x.example.org", Reason: "sni", Time: when}},
	})
	b, err := json.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"Policy":"gateway"`, `"Reason":"class relay"`, `"Kind":"relay"`, `"Origin":"https://relay.example:8443"`,
		`"Operator":"Lab"`, `"State":"up"`, `"Logs":true`, `"Allowlist":[{"Host":"pypi.org","Port":443}]`,
		`"StreamsPerSession":16`, `"Allowed":2`, `"DroppedUDP":5`, `"LastDenied":{"Host":"x.example.org","Reason":"sni","Time":"2023-11-14T22:13:20Z"}`,
	} {
		if !strings.Contains(string(b), want) {
			t.Errorf("missing %s in\n%s", want, b)
		}
	}
}

// No gateway, no last denial: the fields are absent or empty, never invented;
// the allowlist is an empty array, not null.
func TestEgressObjectNone(t *testing.T) {
	b, _ := json.Marshal(egressObject(netdriver.NetworkEgress{Policy: netdriver.PolicyNone, Reason: "engine-internal network"}))
	s := string(b)
	if strings.Contains(s, `"Gateway"`) || strings.Contains(s, `"LastDenied"`) || !strings.Contains(s, `"Allowlist":[]`) {
		t.Fatalf("%s", s)
	}
}
