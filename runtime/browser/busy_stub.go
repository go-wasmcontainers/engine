//go:build !js

package browser

// Label and Mark are the engine worker's busy-log sites (busy_js.go); off the
// browser there is no worker to label.
func Label(string) int { return 0 }

// Mark: see Label.
func Mark(int) {}

// Marked: see Label.
func Marked() int { return 0 }

// updateGC: see Label.
func updateGC() {}
