package browser

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// localTrivialHarness loads worker.js into a bare context, starts it through its
// own onmessage with a stub WebAssembly.instantiate (the imports are captured, no
// module runs), then calls the clock and yield imports as a guest would. A call
// that reached the engine would hang on Atomics.wait; the context has no engine.
const localTrivialHarness = `
const vm = require("vm");
const fs = require("fs");
const posted = [];
const memory = new WebAssembly.Memory({ initial: 1, maximum: 1, shared: true });
let imports;
const sandbox = {
  postMessage: (m) => posted.push(m.t),
  onmessage: null,
  performance,
  WebAssembly: {
    Memory: WebAssembly.Memory,
    instantiate: async (m, i) => { imports = i; return { exports: { memory, _start() {} } }; },
  },
};
const ctx = vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx);
const dv = new DataView(memory.buffer);
const OUT = 512;
function read() { return dv.getBigUint64(OUT, true); }
function clear() { dv.setBigUint64(OUT, 0xffffffffffffffffn, true); }
async function run() {
  const names = ["clock_time_get", "clock_res_get", "sched_yield", "fd_write"];
  const funcs = names.map((n, i) => ["wasi_snapshot_preview1", n, ["clock_time_get", "clock_res_get", "sched_yield", "fd_write"][i] === "x" ? "" : "", "i"]);
  funcs[0][2] = "iIi"; funcs[1][2] = "ii"; funcs[2][2] = ""; funcs[3][2] = "iiii";
  const start = performance.timeOrigin + performance.now() - 5000;
  await sandbox.onmessage({ data: { module: {}, ctl: new SharedArrayBuffer(4096), funcs, origin: start } });
  const w = imports.wasi_snapshot_preview1;
  const out = {};
  out.exited = posted.includes("exit");
  const wallBefore = BigInt(Date.now()) * 1000000n;
  clear(); out.wallErr = w.clock_time_get(0, 1000n, OUT); out.wall = read();
  const wallAfter = BigInt(Date.now()) * 1000000n;
  out.wallOk = out.wall >= wallBefore && out.wall <= wallAfter && out.wall % 1000000n === 0n;
  out.mono = [];
  for (const id of [1, 2, 3]) {
    clear(); const e = w.clock_time_get(id, 1n, OUT); out.mono.push([e, Number(read()) / 1e9]);
  }
  let prev = 0n, back = 0;
  for (let i = 0; i < 20000; i++) {
    clear(); w.clock_time_get(1, 1n, OUT);
    const v = read(); if (v < prev) back++; prev = v;
  }
  out.back = back;
  clear(); out.badId = w.clock_time_get(4, 1n, OUT); out.badIdUntouched = read() === 0xffffffffffffffffn;
  out.badIdHigh = w.clock_time_get(0x80000001, 1n, OUT);
  out.oob = [w.clock_time_get(0, 1n, 65536 - 7), w.clock_time_get(1, 1n, 65536 - 7), w.clock_time_get(0, 1n, -1)];
  out.oobBad = w.clock_time_get(9, 1n, 65536 - 7);
  clear(); out.res = [w.clock_res_get(0, OUT), Number(read())];
  clear(); out.resOther = [w.clock_res_get(77, OUT), Number(read())];
  out.resOob = w.clock_res_get(0, 65536 - 7);
  out.yield = w.sched_yield();
  out.postedTypes = posted.slice().sort().join(",");
  // Without the process origin the monotonic ids are the engine's to answer.
  out.noStart = vm.runInContext("procStart = null; localTrivial('clock_time_get', [1, 1n, 512])", ctx) === undefined;
  out.noStartWall = vm.runInContext("localTrivial('clock_time_get', [0, 1n, 512])", ctx) === 0;
  out.other = vm.runInContext("localTrivial('fd_write', [1, 0, 0, 0])", ctx) === undefined;
  console.log(JSON.stringify(out, (k, v) => typeof v === "bigint" ? v.toString() : v));
}
run().catch((e) => { console.error(e); process.exit(1); });
`

// TestLocalTrivialCalls: clock_time_get, clock_res_get and sched_yield are
// answered by the worker, with the engine's answers (wasi.Proc.Call): the wall
// clock in whole milliseconds, the monotonic ones counting from the process's
// start and never going back, EINVAL for an unknown clock, EFAULT for a pointer
// out of the memory, 1000 ns of resolution for every id. No message reaches the
// engine, whose thread may be busy for 30 ms.
func TestLocalTrivialCalls(t *testing.T) {
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
	if err := os.WriteFile(harness, []byte(localTrivialHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	// A call that goes to the engine blocks for ever here: the timeout is the
	// verdict, not a bound on a slow machine (the run takes milliseconds).
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, node, harness, worker).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v (a call reached the engine and waited?)\n%s", err, b)
	}
	var got struct {
		Exited         bool
		WallErr        int
		WallOk         bool
		Mono           [][2]float64
		Back           int
		BadId          int
		BadIdUntouched bool
		BadIdHigh      int
		Oob            []int
		OobBad         int
		Res            [2]int
		ResOther       [2]int
		ResOob         int
		Yield          int
		PostedTypes    string
		NoStart        bool
		NoStartWall    bool
		Other          bool
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got.WallErr != 0 || !got.WallOk {
		t.Errorf("realtime clock: errno %d, in whole ms within the call: %v", got.WallErr, got.WallOk)
	}
	if len(got.Mono) != 3 {
		t.Fatalf("monotonic clocks: %v", got.Mono)
	}
	for i, m := range got.Mono {
		// The process began 5 s before the worker was told so.
		if m[0] != 0 || m[1] < 4.9 || m[1] > 30 {
			t.Errorf("clock id %d: errno %v, %v s since the process began (want about 5)", i+1, m[0], m[1])
		}
	}
	if got.Back != 0 {
		t.Errorf("the monotonic clock went back %d times", got.Back)
	}
	if got.BadId != 28 || !got.BadIdUntouched || got.BadIdHigh != 28 {
		t.Errorf("unknown clock: errno %d (want 28), memory untouched %v, id 2^31+1: %d", got.BadId, got.BadIdUntouched, got.BadIdHigh)
	}
	if len(got.Oob) != 3 || got.Oob[0] != 21 || got.Oob[1] != 21 || got.Oob[2] != 21 || got.OobBad != 28 {
		t.Errorf("pointer out of memory: %v (want 21 each), unknown clock first: %d (want 28)", got.Oob, got.OobBad)
	}
	if got.Res != [2]int{0, 1000} || got.ResOther != [2]int{0, 1000} || got.ResOob != 21 {
		t.Errorf("clock_res_get: %v, id 77: %v, out of memory: %d", got.Res, got.ResOther, got.ResOob)
	}
	if got.Yield != 0 {
		t.Errorf("sched_yield: %d", got.Yield)
	}
	if got.PostedTypes != "memory" && got.PostedTypes != "exit,memory" {
		t.Errorf("messages posted to the engine: %q (a call went to it)", got.PostedTypes)
	}
	if !got.NoStart || !got.NoStartWall || !got.Other {
		t.Errorf("fallback: no origin -> engine %v, wall without origin local %v, other import -> engine %v", got.NoStart, got.NoStartWall, got.Other)
	}
}
