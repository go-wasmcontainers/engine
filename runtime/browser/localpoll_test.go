package browser

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// localPollHarness loads worker.js into a bare context (no worker, no engine:
// a call to the engine would hang on its Atomics.wait) and drives localPoll
// against a shared memory, printing what it saw as one JSON object.
const localPollHarness = `
const vm = require("vm");
const fs = require("fs");
const posted = [];
const ctx = vm.createContext({ postMessage: (m) => posted.push(m), onmessage: null });
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx);
vm.runInContext("i32 = new Int32Array(new SharedArrayBuffer(1024)); guestMemory = new WebAssembly.Memory({initial: 1, maximum: 1, shared: true});", ctx);
const dv = new DataView(vm.runInContext("guestMemory.buffer", ctx));
const IN = 1000, OUT = 4000, RES = 6000;
// sub writes a subscription: userdata, type, clock id, timeout (ns), flags.
function sub(i, userdata, type, id, timeout, flags) {
  const s = IN + 48 * i;
  new Uint8Array(dv.buffer, s, 48).fill(0);
  dv.setBigUint64(s, BigInt(userdata), true);
  dv.setUint8(s + 8, type);
  dv.setUint32(s + 16, id, true);
  dv.setBigUint64(s + 24, BigInt(timeout), true);
  dv.setUint16(s + 40, flags, true);
}
function poll(n) {
  new Uint8Array(dv.buffer, OUT, 256).fill(0xff);
  dv.setUint32(RES, 0xffffffff, true);
  const t0 = process.hrtime.bigint();
  const r = vm.runInContext("localPoll(" + IN + "," + OUT + "," + n + "," + RES + ")", ctx);
  const ms = Number(process.hrtime.bigint() - t0) / 1e6;
  const events = [];
  for (let i = 0; i < dv.getUint32(RES, true) && i < 4; i++) {
    const o = OUT + 32 * i;
    events.push({ userdata: Number(dv.getBigUint64(o, true)), errno: dv.getUint16(o + 8, true), type: dv.getUint8(o + 10), rest: dv.getBigUint64(o + 16, true) === 0n && dv.getBigUint64(o + 24, true) === 0n });
  }
  return { r, ms, n: dv.getUint32(RES, true), events };
}
const out = {};
sub(0, 42, 0, 1, 5e6, 0);
out.one = poll(1);
sub(0, 1, 0, 1, 1e6, 0); sub(1, 2, 0, 0, 30e6, 0); sub(2, 3, 0, 3, 1e6, 0);
out.three = poll(3);
sub(0, 7, 0, 1, 0, 0);
out.zero = poll(1);
sub(0, 7, 1, 1, 5e6, 0);
out.fd = poll(1);
sub(0, 7, 0, 1, 5e6, 1);
out.abs = poll(1);
sub(0, 7, 0, 9, 5e6, 0);
out.badclock = poll(1);
out.none = poll(0);
out.far = poll(2000);
out.posted = posted.length;
console.log(JSON.stringify(out));
`

// TestLocalPoll: a poll_oneoff made of relative clocks is answered by the
// worker itself, with the events the engine would write, and sleeps the
// earliest timeout; any other poll is left to the engine (null). The worker
// does not wait for the engine's thread to be free to sleep 2 ms.
func TestLocalPoll(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node to run worker.js")
	}
	dir := t.TempDir()
	worker := filepath.Join(dir, "worker.js")
	harness := filepath.Join(dir, "harness.js")
	if err := os.WriteFile(worker, WorkerJS, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(harness, []byte(localPollHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := exec.Command(node, harness, worker).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	type event struct {
		Userdata float64
		Errno    int
		Type     int
		Rest     bool
	}
	type result struct {
		R      *int
		Ms     float64
		N      int
		Events []event
	}
	var got struct {
		One, Three, Zero, Fd, Abs, Badclock, None, Far result
		Posted                                         int
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if r := got.One; r.R == nil || *r.R != 0 || r.N != 1 || len(r.Events) != 1 ||
		r.Events[0] != (event{Userdata: 42, Errno: 0, Type: 0, Rest: true}) || r.Ms < 4.5 || r.Ms > 500 {
		t.Errorf("one 5 ms clock: %+v", r)
	}
	// The earliest clocks fire together (the engine's rule), the later one does not.
	if r := got.Three; r.R == nil || *r.R != 0 || r.N != 2 || len(r.Events) != 2 ||
		r.Events[0].Userdata != 1 || r.Events[1].Userdata != 3 || r.Ms < 0.9 || r.Ms > 25 {
		t.Errorf("three clocks, two earliest: %+v", r)
	}
	if r := got.Zero; r.R == nil || *r.R != 0 || r.N != 1 || r.Ms > 5 {
		t.Errorf("an expired clock: %+v", r)
	}
	for name, r := range map[string]result{"a descriptor": got.Fd, "an absolute deadline": got.Abs, "an unknown clock": got.Badclock, "no subscription": got.None, "too many": got.Far} {
		if r.R != nil {
			t.Errorf("%s was answered by the worker (%d), not left to the engine", name, *r.R)
		}
	}
	if got.Posted != 0 {
		t.Errorf("localPoll posted %d messages to the engine", got.Posted)
	}
}
