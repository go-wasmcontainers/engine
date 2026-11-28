package server

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/netdriver"
)

// Round 2, item 1: a value that cannot be encoded is a 500 with a JSON error,
// never a 200 with an empty body.
func TestWriteJSONEncodeFailureIs500(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusOK, api.EgressLimits{ConnectsPerSecond: math.NaN()})
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, body %q", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content type %q", ct)
	}
	var e api.ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil || e.Message == "" {
		t.Fatalf("body %q: %v", w.Body.String(), err)
	}
}

func TestWriteJSONSuccessKeepsStatusAndBody(t *testing.T) {
	w := httptest.NewRecorder()
	writeJSON(w, http.StatusCreated, map[string]int{"a": 1})
	if w.Code != http.StatusCreated || w.Header().Get("Content-Type") != "application/json" || strings.TrimSpace(w.Body.String()) != `{"a":1}` {
		t.Fatalf("%d %q %q", w.Code, w.Header().Get("Content-Type"), w.Body.String())
	}
	// The error status of a caller is kept when the body encodes.
	w = httptest.NewRecorder()
	writeError(w, http.StatusNotFound, errString("nope"))
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "nope") {
		t.Fatalf("%d %q", w.Code, w.Body.String())
	}
}

type errString string

func (e errString) Error() string { return string(e) }

// Round 2, items 2 and 7: the time labels are RFC 3339; an unknown time is
// absent, not "0001-01-01".
func TestEgressObjectTimes(t *testing.T) {
	at := time.Date(2026, 11, 28, 9, 0, 0, 0, time.UTC)
	o := egressObject(netdriver.NetworkEgress{Policy: netdriver.PolicyGateway, Counters: netdriver.EgressCounters{
		AsOf: at, LastDenied: &netdriver.EgressDenial{Host: "h.example.org"}}})
	if o.Counters.AsOf != "2026-11-28T09:00:00Z" {
		t.Errorf("AsOf %q", o.Counters.AsOf)
	}
	if o.Counters.LastDenied == nil || o.Counters.LastDenied.Time != "" {
		t.Errorf("an unknown denial time was drawn: %+v", o.Counters.LastDenied)
	}
	o = egressObject(netdriver.NetworkEgress{Policy: netdriver.PolicyNone})
	if o.Counters.AsOf != "" {
		t.Errorf("AsOf %q from nothing", o.Counters.AsOf)
	}
	o = egressObject(netdriver.NetworkEgress{Counters: netdriver.EgressCounters{LastDenied: &netdriver.EgressDenial{Host: "h", Time: at}}})
	if o.Counters.LastDenied.Time != "2026-11-28T09:00:00Z" {
		t.Errorf("time %q", o.Counters.LastDenied.Time)
	}
}
