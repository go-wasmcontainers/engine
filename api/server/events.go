package server

import (
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/events"
)

// ParseEventTime reads a since/until value as Docker does: Unix seconds
// (with an optional fraction), RFC 3339, or a Go duration counted back
// from now ("10m").
func ParseEventTime(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	sec, frac, _ := strings.Cut(s, ".")
	n, err := strconv.ParseInt(sec, 10, 64)
	if err != nil || n < 0 || n > math.MaxInt64/int64(time.Second) {
		return time.Time{}, fmt.Errorf("invalid timestamp %q", s)
	}
	var ns int64
	if frac != "" {
		if len(frac) > 9 || strings.Trim(frac, "0123456789") != "" {
			return time.Time{}, fmt.Errorf("invalid timestamp %q", s)
		}
		ns, _ = strconv.ParseInt(frac+strings.Repeat("0", 9-len(frac)), 10, 64)
	}
	return time.Unix(n, ns), nil
}

// eventFilter turns GET /events filters into a predicate: within a key
// any value may match, across keys all must.
func eventFilter(raw string) (func(events.Event) bool, error) {
	if raw == "" {
		return nil, nil
	}
	var f map[string][]string
	if err := json.Unmarshal([]byte(raw), &f); err != nil {
		return nil, fmt.Errorf("invalid filters: %v", err)
	}
	for k := range f {
		switch k {
		case "type", "event", "container", "image", "volume", "label":
		default:
			return nil, fmt.Errorf("unsupported event filter %q", k)
		}
	}
	any := func(vals []string, ok func(string) bool) bool {
		for _, v := range vals {
			if ok(v) {
				return true
			}
		}
		return len(vals) == 0
	}
	return func(e events.Event) bool {
		return any(f["type"], func(v string) bool { return e.Type == v }) &&
			any(f["event"], func(v string) bool { return e.Action == v }) &&
			any(f["container"], func(v string) bool {
				return e.Type == events.Container && (e.Attributes["name"] == v || (v != "" && strings.HasPrefix(e.ID, v)))
			}) &&
			any(f["image"], func(v string) bool {
				return (e.Type == events.Image && (e.ID == v || e.Attributes["name"] == v)) || (e.Type == events.Container && e.Attributes["image"] == v)
			}) &&
			any(f["volume"], func(v string) bool { return e.Type == events.Volume && e.ID == v }) &&
			any(f["label"], func(v string) bool {
				k, val, has := strings.Cut(v, "=")
				got, ok := e.Attributes[k]
				return ok && (!has || got == val)
			})
	}, nil
}

func eventMessage(e events.Event) api.EventMessage {
	attrs := e.Attributes
	if attrs == nil {
		attrs = map[string]string{}
	}
	return api.EventMessage{Type: e.Type, Action: e.Action, Actor: api.EventActor{ID: e.ID, Attributes: attrs},
		Scope: "local", Time: e.Time.Unix(), TimeNano: e.Time.UnixNano()}
}

// systemEvents streams events as JSON, one object per line: the kept ones
// since `since`, then live ones until `until` or the client leaves. A
// client that reads too slowly is cut off (the stream ends).
func (s *server) systemEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	now := time.Now()
	since, err := ParseEventTime(q.Get("since"), now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	until, err := ParseEventTime(q.Get("until"), now)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	filter, err := eventFilter(q.Get("filters"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	past, sub := s.d.Events().Subscribe(since, filter)
	defer sub.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fw := newFlushWriter(w)
	fw.rc.Flush()
	enc := json.NewEncoder(fw)
	for _, e := range past {
		if !until.IsZero() && e.Time.After(until) {
			return
		}
		if enc.Encode(eventMessage(e)) != nil {
			return
		}
	}
	var stop <-chan time.Time
	if !until.IsZero() {
		if !until.After(now) {
			return
		}
		t := time.NewTimer(until.Sub(now))
		defer t.Stop()
		stop = t.C
	}
	for {
		select {
		case e, ok := <-sub.C:
			if !ok {
				return // cut off for reading too slowly
			}
			if enc.Encode(eventMessage(e)) != nil {
				return
			}
		case <-stop:
			return
		case <-r.Context().Done():
			return
		}
	}
}
