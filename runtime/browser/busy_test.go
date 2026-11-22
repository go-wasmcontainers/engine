package browser

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// busyHarness loads busy.js into a bare context with a fake Go glue whose
// _resume runs a scripted piece of work on a fake clock, and prints what the
// log kept.
const busyHarness = `
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
const out = {};
const evOf = (data) => ({ id: 1, this: null, args: [{ data }] });

// 1. A 20 ms slice started by an HTTP request, nothing marked.
go._pendingEvent = evOf({ t: "http.req", method: "GET", url: "/containers/q/logs?stdout=1" });
go.work = () => { now += 20; };
go._resume();
// 2. A 20 ms slice with a labelled site: 5 ms before, 12 under the label, 3 after.
const call = busy.label("call:gpu.queue-submit");
go._pendingEvent = evOf({ t: "call" });
go.work = () => { now += 5; busy.slot[0] = call; now += 12; busy.slot[0] = 0; now += 3; };
go._resume();
// 3. A timer slice of 15 ms, a GC counted inside it.
go._pendingEvent = null;
go.work = () => { now += 15; busy.gcn[0]++; };
go._resume();
// 4. Go calling back into Go: one slice, not two.
go._pendingEvent = evOf({ t: "call" });
go.work = (function () { let first = true; return () => { if (first) { first = false; now += 3; go._resume(); now += 3; } else { now += 2; } }; })();
go._resume();
out.kept = busy.slices(0);
out.slow = ctx.__busySlow.length;
out.written = busy.written();

// 5. Fast slices are not kept and allocate nothing.
go._pendingEvent = evOf({ t: "call" });
go.work = () => { now += 0.2; busy.slot[0] = call; busy.slot[0] = 0; };
if (global.gc) global.gc();
const h0 = process.memoryUsage().heapUsed;
for (let i = 0; i < 300000; i++) go._resume();
if (global.gc) global.gc();
out.heap = process.memoryUsage().heapUsed - h0;
out.gcExposed = !!global.gc;
out.writtenAfter = busy.written();
console.log(JSON.stringify(out));
`

func runNode(t *testing.T, harness string, args ...string) []byte {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node to run the harness")
	}
	dir := t.TempDir()
	h := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(h, []byte(harness), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(node, append([]string{"--expose-gc", h}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	return b
}

func writeAsset(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestBusyLogLabelsEverySlowSlice: a slice of the engine worker that runs 1 ms
// or more is kept with what started it and the time under each Go label; what
// no label covers is "unlabelled", never missing; a slice shorter than that
// is not kept and costs no allocation.
func TestBusyLogLabelsEverySlowSlice(t *testing.T) {
	b := runNode(t, busyHarness, writeAsset(t, "busy.js", []byte(BusyJS)))
	var got struct {
		Kept []struct {
			At, Dur float64
			Trigger string
			Labels  []struct {
				Name string
				Ms   float64
			}
			GC float64
		}
		Slow         int
		Written      int
		Heap         float64
		GcExposed    bool
		WrittenAfter int
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if len(got.Kept) != 4 {
		t.Fatalf("kept %d slices, want 4: %+v", len(got.Kept), got.Kept)
	}
	labelMs := func(i int, name string) float64 {
		for _, l := range got.Kept[i].Labels {
			if l.Name == name {
				return l.Ms
			}
		}
		return -1
	}
	near := func(a, b float64) bool { return math.Abs(a-b) < 1e-6 }
	// 1: the route is the trigger (without its query); all of it unlabelled.
	if k := got.Kept[0]; k.Trigger != "msg:http.req GET /containers/q/logs" || !near(k.Dur, 20) || !near(labelMs(0, "unlabelled"), 20) || k.At != 1e12+100 {
		t.Errorf("slice 1: %+v", k)
	}
	// 2: the message type, the labelled 12 ms and the 8 ms around it.
	if k := got.Kept[1]; k.Trigger != "msg:call" || !near(labelMs(1, "call:gpu.queue-submit"), 12) || !near(labelMs(1, "unlabelled"), 8) {
		t.Errorf("slice 2: %+v", k)
	}
	// 3: no event is a timer (a goroutine woken by Go's own clock); the GC is counted.
	if k := got.Kept[2]; k.Trigger != "timer" || k.GC != 1 || !near(labelMs(2, "unlabelled"), 15) {
		t.Errorf("slice 3: %+v", k)
	}
	// 4: nested resumes are one slice of 8 ms.
	if k := got.Kept[3]; !near(k.Dur, 8) {
		t.Errorf("slice 4: %+v", k)
	}
	if got.Slow != 3 {
		t.Errorf("__busySlow holds %d slices, want the 3 of 10 ms or more", got.Slow)
	}
	if got.WrittenAfter != got.Written {
		t.Errorf("300000 fast slices wrote %d rows", got.WrittenAfter-got.Written)
	}
	if got.GcExposed && got.Heap > 1<<20 {
		t.Errorf("300000 fast slices grew the heap by %.0f bytes: the fast path allocates", got.Heap)
	}
}

// TestLongWaitWithoutLabelIsFlagged: the contract the log exists for. A host
// call's wait is explained by the slices that ran in it; time a slice spent
// outside any label, or that no slice covers, is Unattributed, so a long wait
// nothing names is visible and not silently spread over the named ones.
func TestLongWaitWithoutLabelIsFlagged(t *testing.T) {
	slices := []BusySlice{
		{At: 1000, Dur: 40, Trigger: "msg:call", Labels: []BusyLabel{{"call:gpu.queue-submit", 30}}},
		{At: 1040, Dur: 20, Trigger: "timer", Labels: nil},
	}
	// 1010..1050: 30 of the first slice's 40 ms (22.5 labelled, 7.5 not), 10 of
	// the timer's 20 (all unlabelled).
	got := Explain(1010, 1050, slices)
	var sum float64
	for _, s := range got {
		sum += s.Ms
	}
	if math.Abs(sum-40) > 1e-9 {
		t.Errorf("shares add up to %.3f ms, want the 40 ms wait: %+v", sum, got)
	}
	if got[0].Cause != "msg:call / call:gpu.queue-submit" || math.Abs(got[0].Ms-22.5) > 1e-9 {
		t.Errorf("largest share: %+v", got[0])
	}
	// The message slice is named by its message even where no site marked it;
	// the timer's 10 ms, which nothing started and nothing marked, are not.
	if u := Unattributed(1010, 1050, slices); math.Abs(u-10) > 1e-9 {
		t.Errorf("unattributed = %.3f, want 10", u)
	}
	bare := []BusySlice{{At: 0, Dur: 30, Trigger: "msg:http.req GET /containers/q/logs"}, {At: 30, Dur: 10, Trigger: "callback"}, {At: 40, Dur: 10, Trigger: "other"}}
	if u := Unattributed(0, 60, bare); math.Abs(u-30) > 1e-9 {
		t.Errorf("a route, a bare callback, other, then 10 ms of nothing: unattributed %.3f, want 30", u)
	}
	// A wait no slice covers is all "no slice": flagged, not dropped.
	if u := Unattributed(2000, 2030, slices); u != 30 {
		t.Errorf("a wait with no slice: unattributed %.3f, want 30", u)
	}
	if s := Explain(2000, 2030, slices); len(s) != 1 || s[0].Cause != "no slice" {
		t.Errorf("a wait with no slice: %+v", s)
	}
	// An empty or reversed window explains nothing.
	if s := Explain(1050, 1050, slices); len(s) != 0 {
		t.Errorf("empty window: %+v", s)
	}
	if s := Explain(1050, 1010, slices); len(s) != 0 {
		t.Errorf("reversed window: %+v", s)
	}
	// Equal shares are listed by cause, so a report is the same every run.
	tie := []BusySlice{
		{At: 0, Dur: 10, Trigger: "b", Labels: []BusyLabel{{"x", 10}}},
		{At: 10, Dur: 10, Trigger: "a", Labels: []BusyLabel{{"x", 10}}},
		{At: 20, Dur: 10, Trigger: "c", Labels: []BusyLabel{{"x", 10}}},
		{At: 30, Dur: 10, Trigger: "0", Labels: []BusyLabel{{"x", 10}}},
		{At: 40, Dur: 5, Trigger: "a", Labels: []BusyLabel{{"y", 5}}},
		{At: 45, Dur: 20, Trigger: "z", Labels: []BusyLabel{{"y", 20}}},
	}
	want := []string{"z / y", "0 / x", "a / x", "b / x", "c / x", "a / y"}
	s := Explain(0, 65, tie)
	if len(s) != len(want) {
		t.Fatalf("tied shares: %+v", s)
	}
	for i, w := range want {
		if s[i].Cause != w {
			t.Errorf("share %d is %q, want %q (largest first, ties by cause): %+v", i, s[i].Cause, w, s)
		}
	}
	// Fully labelled: nothing unattributed.
	full := []BusySlice{{At: 0, Dur: 30, Trigger: "msg:call", Labels: []BusyLabel{{"call:x", 30}}}}
	if u := Unattributed(0, 30, full); u != 0 {
		t.Errorf("a fully labelled wait: unattributed %.3f", u)
	}
	if strings.Contains(BusyJS, "console.log") {
		t.Error("busy.js logs")
	}
}

// clockHarness drives worker.js's call() against a simulated engine whose
// clock is d ms behind the guest worker's, and prints the slow message.
const clockHarness = `
const vm = require("vm");
const fs = require("fs");
const D = 3.7; // the guest worker's epoch clock reads D ms more than the engine's
let t = 0;     // true time
const posted = [];
const ctx = vm.createContext({
  onmessage: null,
  performance: { timeOrigin: 0, now: () => t + D },
  postMessage: (m) => {
    posted.push(m);
    if (m.t !== "call") return;
    // the engine: wait, serve, answer; its clock is true time.
    t += sim.wait;
    ctx.view.setFloat64(216, t, true);
    t += sim.exec;
    ctx.view.setFloat64(224, t, true);
    ctx.i32[0] = 2;
    t += sim.back;
  },
  Atomics, SharedArrayBuffer, DataView, Int32Array,
});
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx);
const sim = { wait: 0.05, exec: 0.05, back: 0.05 };
vm.runInContext("i32 = new Int32Array(new SharedArrayBuffer(1024)); view = new DataView(i32.buffer); callNames[0] = 'x';", ctx);
ctx.i32 = vm.runInContext("i32", ctx);
ctx.view = vm.runInContext("view", ctx);
for (let i = 0; i < 20; i++) { vm.runInContext("call(0, '', '', [])", ctx); t += 1; }
sim.wait = 30; sim.exec = 5;
vm.runInContext("call(0, '', '', [])", ctx);
console.log(JSON.stringify(posted.filter((m) => m.t === "slow")));
`

// TestSlowCallSplitIsOnOneClock: the two workers' epoch clocks differ (here by
// 3.7 ms, what the CI run measured); a call that waited 30 ms for the engine
// and returned at once must say wait 30, return 0.05, not wait 26 and return
// 3.7 -- and carry the offset it corrected.
func TestSlowCallSplitIsOnOneClock(t *testing.T) {
	b := runNode(t, clockHarness, writeAsset(t, "worker.js", WorkerJS))
	var got []struct {
		At, Total, Wait, Exec, Ret, Off float64
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if len(got) != 1 {
		t.Fatalf("slow messages: %s", b)
	}
	s := got[0]
	near := func(a, b float64) bool { return math.Abs(a-b) < 0.02 }
	if !near(s.Wait, 30) || !near(s.Exec, 5) || !near(s.Ret, 0.05) || !near(s.Off, 3.7) || !near(s.Total, 35.05) {
		t.Errorf("split = %+v, want wait 30, exec 5, return 0.05, offset 3.7, total 35.05", s)
	}
}
