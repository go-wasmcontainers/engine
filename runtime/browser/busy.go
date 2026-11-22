package browser

import (
	_ "embed"
	"sort"
	"strings"
)

// BusyJS is the engine worker's busy log (busy.js): it wraps Go's event resume,
// times every slice of the engine's work and publishes the slow ones,
// labelled, as self.__busySlow beside self.__hostSlow.
//
// The host serves it as a file and the engine worker loads it with
// importScripts after wasm_exec.js and before Go runs (New only picks up
// self.__busy). It is never evaluated from a string: the page's
// Content-Security-Policy has no 'unsafe-eval', so Function(...) would panic
// the engine at start.
//
//go:embed busy.js
var BusyJS string

// BusyLabel is time a slice spent under one label.
type BusyLabel struct {
	Name string
	Ms   float64
}

// BusySlice is one slice of the engine worker's work as busy.js recorded it:
// when it began (epoch ms), how long it ran, what started it, the labels the
// Go sites gave it (the slice's rest is "unlabelled") and the garbage
// collections that ran in it.
type BusySlice struct {
	At, Dur float64
	Trigger string
	Labels  []BusyLabel
	GC      int
	// GCMs is the garbage collector's CPU time (ms) that accrued during a slice
	// of 10 ms or more (runtime/metrics /cpu/classes/gc/total, read by busy.js
	// at the slice's end); 0 for a shorter one.
	GCMs float64
}

// Share is the part of a wait one cause accounts for.
type Share struct {
	Cause string // "trigger / label", or "no slice" for time nothing recorded
	Ms    float64
}

// Explain attributes the wait [from, to] (epoch ms: the time between a guest
// posting a host call and the engine beginning it) to the slices that ran in
// it. A slice overlapping the wait counts for the overlap, split between its
// labels in proportion; time under no recorded slice is "no slice" (the
// browser's own work in the worker, a slice shorter than busy.js records, or
// a clock that disagrees). The shares are sorted, the largest first, and add
// up to the wait.
func Explain(from, to float64, slices []BusySlice) []Share {
	by := map[string]float64{}
	covered := 0.0
	for _, s := range slices {
		lo, hi := max(from, s.At), min(to, s.At+s.Dur)
		if hi <= lo || s.Dur <= 0 {
			continue
		}
		f := (hi - lo) / s.Dur
		covered += hi - lo
		named := 0.0
		for _, l := range s.Labels {
			by[s.Trigger+" / "+l.Name] += l.Ms * f
			named += l.Ms
		}
		// The recorded labels are the largest few: what they do not add up to
		// of the slice is unlabelled.
		if rest := s.Dur - named; rest > 0 {
			by[s.Trigger+" / unlabelled"] += rest * f
		}
	}
	if rest := to - from - covered; rest > 1e-9 {
		by["no slice"] += rest
	}
	out := make([]Share, 0, len(by))
	for c, ms := range by {
		out = append(out, Share{c, ms})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Ms != out[j].Ms {
			return out[i].Ms > out[j].Ms
		}
		return out[i].Cause < out[j].Cause
	})
	return out
}

// Unattributed is the part of a wait [from, to] that nothing names: time no
// recorded slice covers, and time in a slice that neither a message, a route
// nor an event started (a "timer" or a bare callback: some goroutine of the
// engine) and no Go site labelled. A slice a message started is named by its
// message, even where no site marked it. A long wait with a high Unattributed
// has no label.
func Unattributed(from, to float64, slices []BusySlice) float64 {
	u := 0.0
	for _, s := range Explain(from, to, slices) {
		if s.Cause == "no slice" {
			u += s.Ms
			continue
		}
		if !strings.HasSuffix(s.Cause, " / unlabelled") {
			continue
		}
		switch strings.TrimSuffix(s.Cause, " / unlabelled") {
		case "timer", "callback", "other":
			u += s.Ms
		}
	}
	return u
}

// SliceClass is what the slices of one class (with or without a garbage
// collection in them) add up to.
type SliceClass struct {
	N          int
	Ms         float64 // their durations
	Unlabelled float64 // the part under no label
	Max        float64 // the longest
	GCMs       float64 // the collector's CPU time in them (BusySlice.GCMs)
}

// GCStats splits the slices of minDur ms or more by whether a garbage
// collection ran in them. The engine runs Go's collector on its one thread: if
// the long slices are the ones with a collection, and what they spend is under
// no label (the collector's work is not a Mark site), the unlabelled time is
// the collector's. Slices without one that are as long are something else.
type GCStats struct {
	With, Without SliceClass
}

// GCSplit computes GCStats.
func GCSplit(slices []BusySlice, minDur float64) GCStats {
	var g GCStats
	for _, s := range slices {
		if s.Dur < minDur {
			continue
		}
		c := &g.Without
		if s.GC > 0 {
			c = &g.With
		}
		named := 0.0
		for _, l := range s.Labels {
			if l.Name != "unlabelled" { // busy.js's name for the rest of a slice
				named += l.Ms
			}
		}
		c.N++
		c.Ms += s.Dur
		c.GCMs += s.GCMs
		c.Unlabelled += max(0, s.Dur-named)
		c.Max = max(c.Max, s.Dur)
	}
	return g
}
