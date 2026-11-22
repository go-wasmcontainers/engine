package browser

import "testing"

// TestGCSplit: slices of 10 ms or more are counted apart by whether a collection
// ran in them, with their time and the part under no label.
func TestGCSplit(t *testing.T) {
	slices := []BusySlice{
		{Dur: 33, GC: 1, Labels: []BusyLabel{{"a", 3}}},
		{Dur: 31, GC: 2},
		{Dur: 12, GC: 0, Labels: []BusyLabel{{"a", 12}}},
		{Dur: 9.9, GC: 1}, // under the bound
		{Dur: 80, GC: 0, Labels: []BusyLabel{{"b", 20}, {"c", 10}}},
	}
	got := GCSplit(slices, 10)
	if want := (SliceClass{N: 2, Ms: 64, Unlabelled: 61, Max: 33}); got.With != want {
		t.Errorf("with a collection: %+v, want %+v", got.With, want)
	}
	if want := (SliceClass{N: 2, Ms: 92, Unlabelled: 50, Max: 80}); got.Without != want {
		t.Errorf("without: %+v, want %+v", got.Without, want)
	}
}

// TestGCSplitCountsTheUnlabelledLabel: busy.js names the rest of a slice
// "unlabelled" as a label of its own, so its time is under no Go label. The CI
// of 2026-11-22 printed "0 ms unlabelled" for slices that were all of it.
// The slice's GC time (GCMs) is summed apart.
func TestGCSplitCountsTheUnlabelledLabel(t *testing.T) {
	slices := []BusySlice{
		{Dur: 337, Labels: []BusyLabel{{"unlabelled", 336.6}, {"call:x", 0.4}}},
		{Dur: 27, GC: 1, GCMs: 24, Labels: []BusyLabel{{"unlabelled", 24}, {"call:y", 3}}},
	}
	got := GCSplit(slices, 10)
	if want := (SliceClass{N: 1, Ms: 337, Unlabelled: 336.6, Max: 337}); got.Without != want {
		t.Errorf("without: %+v, want %+v", got.Without, want)
	}
	if want := (SliceClass{N: 1, Ms: 27, Unlabelled: 24, Max: 27, GCMs: 24}); got.With != want {
		t.Errorf("with: %+v, want %+v", got.With, want)
	}
}
