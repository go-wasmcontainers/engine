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

// gpuWorkerHarness loads worker.js into a bare context with TWO stub servers:
// the engine, which STALLS 150 ms on every call it serves (a garbage collection,
// a 300 ms present), and the GPU host worker, which answers at once. A guest's
// imports of a module named in the start message's "remote" go to the host
// worker through the port and the second control block; every other import goes
// to the engine through the first.
const gpuWorkerHarness = `
const vm = require("vm");
const fs = require("fs");
const memory = new WebAssembly.Memory({ initial: 1, maximum: 1, shared: true });
const ctl = new SharedArrayBuffer(4096);
const ctl2 = new SharedArrayBuffer(4096);
const gdv = new DataView(memory.buffer);
const FN = { fd_write: 0, submit: 1, present: 2, list: 3 };
const engineCalls = [0, 0, 0, 0];
const hostCalls = [0, 0, 0, 0];
const enginePosted = [];
const portPosted = [];
function stall(ms) { const t = performance.now(); while (performance.now() - t < ms) {} }
function answer(c, ret) {
  const i32 = new Int32Array(c), dv = new DataView(c);
  if (ret !== undefined) dv.setBigUint64(200, BigInt(ret), true);
  i32[2] = 0; i32[3] = 0;
  Atomics.store(i32, 0, 2);
}
let reallocPending = false;
// The host worker's "ready" reaches the guest on the port, after 0 ms, or after
// 200 ms in the "late" mode: Firefox drops a message carrying a
// SharedArrayBuffer sent before the receiver owns the port (engine#84), so the
// guest must not post its attach before it has heard "ready".
let readyHeard = false, attachEarly = false;
const port = {
  set onmessage(f) {
    if (typeof f !== "function") return;
    setTimeout(() => { readyHeard = true; f({ data: { t: "ready" } }); }, process.argv[3] === "late" ? 200 : 0);
  },
  postMessage: (m) => {
    portPosted.push(m.t);
    if (m.t === "attach" && !readyHeard) attachEarly = true;
    const i32 = new Int32Array(ctl2);
    if (m.t === "call") {
      const fn = i32[1];
      hostCalls[fn]++;
      if (fn === FN.list) { // the host asks the guest for memory first
        reallocPending = true;
        i32[3] = 2; i32[8] = 16; i32[7] = 4;
        Atomics.store(i32, 0, 2);
        return;
      }
      answer(ctl2, 100 + fn);
    } else if (m.t === "realloc") {
      if (!reallocPending) throw new Error("realloc without a request");
      reallocPending = false;
      gdv.setUint32(2048, i32[9], true); // what the host saw as the block
      answer(ctl2, 77);
    }
  },
};
const sandbox = {
  postMessage: (m) => {
    enginePosted.push(m.t);
    if (m.t !== "call") return;
    const i32 = new Int32Array(ctl);
    engineCalls[i32[1]]++;
    stall(150);
    answer(ctl, 0);
  },
  onmessage: null,
  performance,
  setTimeout,
  clearTimeout,
  WebAssembly: {
    Memory: WebAssembly.Memory,
    instantiate: async (m, i) => { imports = i; return { exports: { memory, cabi_realloc: () => 1024, _start() {} } }; },
  },
};
let imports;
const ctx = vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx);
async function run() {
  const funcs = [
    ["wasi_snapshot_preview1", "fd_write", "iiii", "i"],
    ["wasi:webgpu/queue", "submit", "ii", "i"],
    ["wasi-gfx:surface/surface", "present", "ii", "i"],
    ["wasi:webgpu/queue", "list", "ii", "i"],
  ];
  const remote = process.argv[3] === "local" ? undefined : { ctl: ctl2, port, mods: ["wasi:webgpu/queue", "wasi-gfx:surface/surface"] };
  await sandbox.onmessage({ data: { module: {}, ctl, funcs, remote } });
  const q = imports["wasi:webgpu/queue"], s = imports["wasi-gfx:surface/surface"], w = imports.wasi_snapshot_preview1;
  const out = {};
  const t0 = performance.now();
  const r = [];
  for (let n = 0; n < 10; n++) r.push(q.submit(1, 2), s.present(3, 4));
  out.gpuMs = performance.now() - t0;
  out.results = r.slice(0, 2);
  out.list = q.list(5, 6);
  out.realloc = gdv.getUint32(2048, true);
  out.engineCalls = engineCalls.slice();
  out.hostCalls = hostCalls.slice();
  out.afterGPUFd = w.fd_write(1, 2, 3, 4);
  out.engineCallsAfter = engineCalls.slice();
  out.portPosted = portPosted.filter((t, i) => portPosted.indexOf(t) === i);
  out.attachEarly = attachEarly;
  console.log(JSON.stringify(out));
}
run().catch((e) => { console.error(e); process.exit(1); });
`

func runGPUWorker(t *testing.T, mode string) []byte {
	t.Helper()
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
	if err := os.WriteFile(harness, []byte(gpuWorkerHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, node, harness, worker, mode).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	return b
}

type gpuWorkerResult struct {
	GpuMs            float64
	Results          []int
	List, Realloc    int
	EngineCalls      []int
	HostCalls        []int
	AfterGPUFd       int
	EngineCallsAfter []int
	PortPosted       []string
	AttachEarly      bool
}

// TestAttachWaitsForTheHostWorkersReady: the host worker says "ready" 200 ms
// late; the guest's attach (its memory and control block, both
// SharedArrayBuffers) must not be posted before that. Red on a worker that
// posts it at once: Firefox then drops the message and the guest hangs on its
// first GPU call (go-wasmcontainers/engine#84).
func TestAttachWaitsForTheHostWorkersReady(t *testing.T) {
	var got gpuWorkerResult
	b := runGPUWorker(t, "late")
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got.AttachEarly {
		t.Errorf("attach posted before the host worker's ready\n%s", b)
	}
	if got.HostCalls[1] != 10 {
		t.Errorf("host calls %v, want the guest to run after the late ready\n%s", got.HostCalls, b)
	}
}

// TestGPUCallsDoNotWaitForTheEngine: with an engine that stalls 150 ms on every
// call it serves, 20 calls into the modules named "remote" take far less than
// 100 ms, none of them reaches the engine, the host worker's answers are the
// calls' results, a host call that needs the guest's cabi_realloc is served on
// the port, and an import of another module still goes to the engine. Red on a
// worker that sends every import to the engine: 20 x 150 ms.
func TestGPUCallsDoNotWaitForTheEngine(t *testing.T) {
	var got gpuWorkerResult
	b := runGPUWorker(t, "remote")
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got.GpuMs >= 100 {
		t.Errorf("20 GPU calls took %.0f ms with the engine stalled, want under 100\n%s", got.GpuMs, b)
	}
	if got.EngineCalls[1]+got.EngineCalls[2]+got.EngineCalls[3] != 0 {
		t.Errorf("GPU calls reached the engine: %v\n%s", got.EngineCalls, b)
	}
	if got.HostCalls[1] != 10 || got.HostCalls[2] != 10 || got.HostCalls[3] != 1 || got.HostCalls[0] != 0 {
		t.Errorf("host worker calls %v, want [0 10 10 1]\n%s", got.HostCalls, b)
	}
	if len(got.Results) != 2 || got.Results[0] != 101 || got.Results[1] != 102 {
		t.Errorf("results %v, want [101 102] (the host worker's answers)", got.Results)
	}
	if got.List != 77 || got.Realloc != 1024 {
		t.Errorf("list %d, realloc block %d, want 77 and 1024 (the guest's cabi_realloc served on the port)\n%s", got.List, got.Realloc, b)
	}
	if got.EngineCallsAfter[0] != 1 {
		t.Errorf("a WASI import must still reach the engine: %v", got.EngineCallsAfter)
	}
}

// TestGPUCallsStayWithTheEngineWithoutAHostWorker: no "remote" in the start
// message (the page made no GPU host worker), every import is the engine's, as
// before.
func TestGPUCallsStayWithTheEngineWithoutAHostWorker(t *testing.T) {
	var got gpuWorkerResult
	b := runGPUWorker(t, "local")
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got.EngineCalls[1] != 10 || got.EngineCalls[2] != 10 || got.HostCalls[1] != 0 {
		t.Errorf("engine calls %v, host calls %v, want all 21 GPU calls on the engine", got.EngineCalls, got.HostCalls)
	}
}
