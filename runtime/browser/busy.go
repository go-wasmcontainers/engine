package browser

import (
	_ "embed"
	"sort"
	"strings"
)

// BusyJS is the engine worker's busy log (busy.js), installed by New on
// GOOS=js: it wraps Go's event resume, times every slice of the engine's work
// and publishes the slow ones, labelled, as self.__busySlow beside
// self.__hostSlow.
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
	if to <= from {
		return nil
	}
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

// Unattributed is the part of a wait [from, to] that no recorded slice
// covers, or that a slice covers without a name: the time the labels do not
// explain. A long wait with a high Unattributed has no label.
func Unattributed(from, to float64, slices []BusySlice) float64 {
	u := 0.0
	for _, s := range Explain(from, to, slices) {
		if s.Cause == "no slice" || strings.HasSuffix(s.Cause, " / unlabelled") {
			u += s.Ms
		}
	}
	return u
}
