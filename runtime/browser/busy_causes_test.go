package browser

import (
	"encoding/json"
	"math"
	"testing"
)

func shareMs(shares []Share, cause string) float64 {
	for _, s := range shares {
		if s.Cause == cause {
			return s.Ms
		}
	}
	return 0
}

func nearMs(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// A wait the log's slices do not cover, while the worker's event loop ran no
// timer for 20 ms of it, is attributed to "worker not scheduled" for those 20 ms.
func TestExplainNamesAWorkerNotScheduled(t *testing.T) {
	ev := Evidence{LateTicks: []Interval{{1005, 1025}}}
	got := ExplainWith(1000, 1030, ev)
	if g := shareMs(got, CauseNotScheduled); !nearMs(g, 20) {
		t.Errorf("%q = %.3f ms, want 20: %+v", CauseNotScheduled, g, got)
	}
	if g := shareMs(got, "no slice"); !nearMs(g, 10) {
		t.Errorf("no slice = %.3f ms, want the 10 the tick does not cover: %+v", g, got)
	}
	if u := UnattributedWith(1000, 1030, ev); !nearMs(u, 10) {
		t.Errorf("unattributed = %.3f, want 10", u)
	}
}

// A wait a message was in flight for is attributed to "message in flight" for
// the measured transit.
func TestExplainNamesAMessageInFlight(t *testing.T) {
	ev := Evidence{Transits: []Interval{{1000, 1027}}}
	got := ExplainWith(1000, 1030, ev)
	if g := shareMs(got, CauseInFlight); !nearMs(g, 27) {
		t.Errorf("%q = %.3f ms, want 27: %+v", CauseInFlight, g, got)
	}
	if u := UnattributedWith(1000, 1030, ev); !nearMs(u, 3) {
		t.Errorf("unattributed = %.3f, want the 3 ms after the transit", u)
	}
}

// Measurements that lie elsewhere explain nothing: the wait stays unexplained
// and still counts as unattributed.
func TestExplainLeavesAnUncoveredWaitUnexplained(t *testing.T) {
	ev := Evidence{LateTicks: []Interval{{2000, 2050}}, Transits: []Interval{{3000, 3040}}}
	got := ExplainWith(1000, 1030, ev)
	if g := shareMs(got, "no slice"); !nearMs(g, 30) {
		t.Errorf("no slice = %.3f ms, want all 30: %+v", g, got)
	}
	if g := shareMs(got, CauseNotScheduled) + shareMs(got, CauseInFlight); g != 0 {
		t.Errorf("a measurement outside the wait was credited %.3f ms: %+v", g, got)
	}
	if u := UnattributedWith(1000, 1030, ev); !nearMs(u, 30) {
		t.Errorf("unattributed = %.3f, want 30", u)
	}
}

// A measurement that only partly overlaps the wait, or the slices, is credited
// for the overlap and never for the whole wait; time is counted once.
func TestExplainCreditsOnlyTheOverlap(t *testing.T) {
	ev := Evidence{
		Slices:    []BusySlice{{At: 1008, Dur: 10, Trigger: "msg:call"}},
		LateTicks: []Interval{{990, 1010}},  // 1000-1008 is free of slices: 8 ms
		Transits:  []Interval{{1020, 1100}}, // 1020-1030: 10 ms
	}
	got := ExplainWith(1000, 1030, ev)
	want := map[string]float64{CauseNotScheduled: 8, CauseInFlight: 10, "msg:call / unlabelled": 10, "no slice": 2}
	sum := 0.0
	for _, s := range got {
		sum += s.Ms
		if w, ok := want[s.Cause]; !ok || !nearMs(s.Ms, w) {
			t.Errorf("%q = %.3f ms, want %v: %+v", s.Cause, s.Ms, want[s.Cause], got)
		}
	}
	if !nearMs(sum, 30) {
		t.Errorf("the shares add up to %.3f ms, want the 30 of the wait", sum)
	}
	// The same instant measured twice is one cause (the late tick first).
	both := Evidence{LateTicks: []Interval{{1000, 1030}}, Transits: []Interval{{1000, 1030}}}
	got = ExplainWith(1000, 1030, both)
	if a, b := shareMs(got, CauseNotScheduled), shareMs(got, CauseInFlight); !nearMs(a, 30) || b != 0 {
		t.Errorf("time under both counted twice: not scheduled %.3f, in flight %.3f", a, b)
	}
}

const busyCausesHarness = `
const vm = require("vm");
const fs = require("fs");
let now = 100;
class Go {
  constructor() { this._pendingEvent = null; this.work = () => {}; }
  _resume() { this.work(); }
}
const ctx = vm.createContext({ Go, performance: { timeOrigin: 1e12, now: () => now } });
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx);
const go = new ctx.Go();
const busy = ctx.__busy;
const tick = () => { if (busy.tick) busy.tick(); };
const evOf = (data) => ({ id: 1, this: null, args: [{ data }] });
// The probe ticks on time, then the loop does not run for 38 ms, then ticks on time.
now = 102; tick();
now = 140; tick();
now = 142; tick();
// A call posted at 1e12+120 reaches Go at 150: 30 ms in flight.
now = 150;
go._pendingEvent = evOf({ t: "call", sent: 1e12 + 120 });
go.work = () => { now += 1; };
go._resume();
// One that took 0.5 ms, one with no stamp and a timer: none is a transit.
now = 200;
go._pendingEvent = evOf({ t: "call", sent: 1e12 + 199.5 });
go._resume();
go._pendingEvent = evOf({ t: "call" });
go._resume();
go._pendingEvent = null;
go._resume();
console.log(JSON.stringify({ late: busy.lateTicks ? busy.lateTicks() : null, transits: busy.transits ? busy.transits() : null }));
`

// TestBusyLogMeasuresLateTicksAndTransits: busy.js records the intervals its
// event loop ran no timer, and the [posted, received] interval of a message
// that took 1 ms or more to reach Go.
func TestBusyLogMeasuresLateTicksAndTransits(t *testing.T) {
	b := runNode(t, busyCausesHarness, writeAsset(t, "busy.js", []byte(BusyJS)))
	var got struct {
		Late     []Interval
		Transits []Interval
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if len(got.Late) != 1 || got.Late[0].From != 1e12+104 || got.Late[0].To != 1e12+140 {
		t.Errorf("late ticks = %+v, want [[1e12+104, 1e12+140]] (the loop's gap from the due tick to the late one)\n%s", got.Late, b)
	}
	if len(got.Transits) != 1 || got.Transits[0].From != 1e12+120 || got.Transits[0].To != 1e12+150 {
		t.Errorf("transits = %+v, want [[1e12+120, 1e12+150]]\n%s", got.Transits, b)
	}
}
