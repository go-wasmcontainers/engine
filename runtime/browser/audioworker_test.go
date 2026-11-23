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

// audioWorkerHarness loads worker.js into a bare context with a stub engine and
// drives wctr_audio_out as a guest would. The stub engine answers a call at
// once when it is stream_open (it adopts the ring the worker posted, as the
// browser provider does) and STALLS for 150 ms on every other call, as the real
// engine's thread does behind a garbage collection or a 300 ms present: a
// stream_write that reaches it is a feed that waited. The page is played by the
// harness: it advances the heartbeat and the read index of the ring.
const audioWorkerHarness = `
const vm = require("vm");
const fs = require("fs");
const memory = new WebAssembly.Memory({ initial: 1, maximum: 1, shared: true });
const ctl = new SharedArrayBuffer(4096);
const i32 = new Int32Array(ctl);
const cdv = new DataView(ctl);
const gdv = new DataView(memory.buffer);
const FN = { fd_write: 0, stream_open: 1, stream_write: 2, stream_stats: 3, stream_close: 4 };
const engineCalls = [0, 0, 0, 0, 0];
const posted = [];
let adopt = process.argv[3] !== "noadopt";
let lastSab = null;
function stall(ms) { const t = performance.now(); while (performance.now() - t < ms) {} }
let imports;
const sandbox = {
  postMessage: (m) => {
    posted.push(m.t);
    if (m.t === "audioring") { lastSab = m.sab; return; }
    if (m.t !== "call") return;
    const fn = i32[1];
    engineCalls[fn]++;
    if (fn === FN.stream_open) {
      const out = Number(cdv.getBigUint64(64 + 8 * 3, true));
      gdv.setUint32(out, 7, true); // the handle
      // The provider takes the ring the worker posted, or allocates its own.
      if (!lastSab) lastSab = new SharedArrayBuffer(64 + Number(cdv.getBigUint64(64 + 8 * 2, true)) * Number(cdv.getBigUint64(64 + 8, true)) * 4);
      else if (adopt) Atomics.store(new Int32Array(lastSab, 0, 16), 8, 1);
    } else {
      stall(150);
    }
    cdv.setBigUint64(200, 0n, true); // the result: 0
    i32[2] = 0; i32[3] = 0;
    Atomics.store(i32, 0, 2);
  },
  onmessage: null,
  performance,
  WebAssembly: {
    Memory: WebAssembly.Memory,
    instantiate: async (m, i) => { imports = i; return { exports: { memory, _start() {} } }; },
  },
};
const ctx = vm.createContext(sandbox);
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), ctx);
const PCM = 4096, REC = 16, OUT = 32;
function put(frames, ch, f) {
  for (let k = 0; k < frames * ch; k++) gdv.setFloat32(PCM + 4 * k, f(k), true);
}
async function run() {
  const funcs = [
    ["wasi_snapshot_preview1", "fd_write", "iiii", "i"],
    ["wctr_audio_out", "stream_open", "iiii", "i"],
    ["wctr_audio_out", "stream_write", "iiii", "i"],
    ["wctr_audio_out", "stream_stats", "ii", "i"],
    ["wctr_audio_out", "stream_close", "i", "i"],
  ];
  await sandbox.onmessage({ data: { module: {}, ctl, funcs } });
  const a = imports.wctr_audio_out;
  const out = {};
  out.open = a.stream_open(11025, 2, 256, 0);
  const handle = gdv.getUint32(0, true);
  out.handle = handle;
  const ring = lastSab, hdr = new Int32Array(ring, 0, 16), data = new Float32Array(ring, 64);
  const rec = () => [gdv.getUint32(REC, true), gdv.getUint32(REC + 4, true)];
  const callsAfterOpen = engineCalls.slice();

  // Waiting: no heartbeat yet. Accepted and dropped, nothing queued.
  put(16, 2, () => 0.5);
  out.waitingErr = a.stream_write(handle, PCM, 16, REC);
  out.waiting = rec();
  out.waitingWrite = Atomics.load(hdr, 0);

  // The page plays: a heartbeat. 100 writes of 8 frames, as a game's feed.
  Atomics.add(hdr, 6, 1);
  const t0 = performance.now();
  let accepted = 0, errs = 0, lastQueued = 0;
  for (let n = 0; n < 20; n++) {
    put(8, 2, (k) => (n * 16 + k) / 1000);
    errs += a.stream_write(handle, PCM, 8, REC);
    accepted += rec()[0];
    lastQueued = rec()[1];
  }
  out.feedMs = performance.now() - t0;
  out.feedErrs = errs;
  out.accepted = accepted;
  out.queued = lastQueued;
  out.writeIdx = Atomics.load(hdr, 0);
  let ok = true;
  for (let k = 0; k < 20 * 16; k++) if (Math.abs(data[k] - k / 1000) > 1e-6) ok = false;
  out.dataOk = ok;

  // The page consumes 100 frames; the writer wraps the 256-frame ring in order.
  Atomics.store(hdr, 1, 100);
  Atomics.store(hdr, 3, 100);
  put(200, 2, (k) => 0.25);
  out.wrapErr = a.stream_write(handle, PCM, 200, REC);
  out.wrap = rec(); // 160 + 200 - 100 = 260 > 256: 4 frames refused
  out.overrun = Atomics.load(hdr, 4);
  out.refusedHeader = Atomics.load(hdr, 7);
  out.wrapIdx = Atomics.load(hdr, 0);

  // Sanitising: NaN and the infinities are 0, the rest is clamped to [-1, 1].
  Atomics.store(hdr, 1, Atomics.load(hdr, 0)); // the page drained it
  put(4, 1 + 1, (k) => [NaN, Infinity, -Infinity, 5, -5, 0.5, 1, -1][k]);
  a.stream_write(handle, PCM, 4, REC);
  const base = ((Atomics.load(hdr, 0) - 4) & 255) * 2;
  out.sanitised = Array.from(data.slice(base, base + 8));

  // Errnos, in the engine's order.
  out.tooLarge = a.stream_write(handle, PCM, 257, REC);
  out.faultOut = a.stream_write(handle, PCM, 1, 65536 - 4);
  out.faultPtr = a.stream_write(handle, 65536 - 4, 8, REC);
  out.stats = [a.stream_stats(handle, 64), gdv.getUint32(64, true), gdv.getUint32(68, true), gdv.getUint32(72, true), gdv.getUint32(76, true)];
  out.statsFault = a.stream_stats(handle, 65536 - 8);

  // The page drops the ring: writes are refused (nothing is accepted), state 2.
  Atomics.store(hdr, 5, 1);
  out.closedErr = a.stream_write(handle, PCM, 8, REC);
  out.closed = rec();
  a.stream_stats(handle, 64);
  out.closedState = gdv.getUint32(76, true);
  Atomics.store(hdr, 5, 0);

  out.callsDuringFeed = engineCalls.map((n, i) => n - callsAfterOpen[i]);
  out.callsToEngineFor = engineCalls.slice();

  // A heartbeat that stood still for 200 ms is waiting again.
  stall(210);
  a.stream_write(handle, PCM, 8, REC);
  out.stale = rec();

  // stream_close is the engine's; afterwards the handle is no longer the worker's.
  a.stream_close(handle);
  out.afterCloseGoesToEngine = (a.stream_write(handle, PCM, 8, REC), engineCalls[FN.stream_write]);
  out.posted = posted.filter((t) => t === "audioring").length;
  console.log(JSON.stringify(out));
}
run().catch((e) => { console.error(e); process.exit(1); });
`

func runAudioWorker(t *testing.T, mode string) []byte {
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
	if err := os.WriteFile(harness, []byte(audioWorkerHarness), 0o644); err != nil {
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

// TestAudioFeedDoesNotWaitForTheEngine: with an engine that stalls 150 ms on every
// call it serves, 20 stream_write calls take no time at all, none of them reaches
// the engine, and the ring holds exactly what a feed of 20 chunks wrote, with the
// engine's semantics (errnos, waiting before the page plays, sanitising, wrap,
// overrun counted as refused). Red on a worker that sends stream_write to the
// engine: 20 x 150 ms.
func TestAudioFeedDoesNotWaitForTheEngine(t *testing.T) {
	b := runAudioWorker(t, "adopt")
	var got struct {
		Open, Handle                    int
		WaitingErr                      int
		Waiting                         [2]int
		WaitingWrite                    int
		FeedMs                          float64
		FeedErrs, Accepted, Queued      int
		WriteIdx                        int
		DataOk                          bool
		WrapErr                         int
		Wrap                            [2]int
		Overrun, RefusedHeader, WrapIdx int
		Sanitised                       []float64
		TooLarge, FaultOut, FaultPtr    int
		Stats                           [5]int
		StatsFault                      int
		ClosedErr                       int
		Closed                          [2]int
		ClosedState                     int
		CallsDuringFeed                 []int
		Stale                           [2]int
		AfterCloseGoesToEngine, Posted  int
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got.Open != 0 || got.Handle != 7 || got.Posted != 1 {
		t.Fatalf("open: errno %d handle %d, rings posted %d\n%s", got.Open, got.Handle, got.Posted, b)
	}
	if got.WaitingErr != 0 || got.Waiting != [2]int{16, 0} || got.WaitingWrite != 0 {
		t.Errorf("before the page plays: errno %d, accepted/queued %v, write index %d (want 0, [16 0], 0)", got.WaitingErr, got.Waiting, got.WaitingWrite)
	}
	if got.CallsDuringFeed[2] != 0 || got.CallsDuringFeed[3] != 0 {
		t.Errorf("host calls that reached the engine: stream_write %d, stream_stats %d (want 0): the audio feed waited behind the engine's thread", got.CallsDuringFeed[2], got.CallsDuringFeed[3])
	}
	if got.FeedMs > 100 {
		t.Errorf("20 writes took %.0f ms with a stalling engine (want well under one stall of 150 ms)", got.FeedMs)
	}
	if got.FeedErrs != 0 || got.Accepted != 160 || got.Queued != 160 || got.WriteIdx != 160 || !got.DataOk {
		t.Errorf("feed: errs %d accepted %d queued %d write index %d data ok %v (want 0 160 160 160 true)", got.FeedErrs, got.Accepted, got.Queued, got.WriteIdx, got.DataOk)
	}
	// 160 written, 100 read: room for 196; 200 offered, 196 accepted, 4 refused.
	if got.WrapErr != 0 || got.Wrap[0] != 196 || got.Wrap[1] != 256 || got.Overrun != 4 || got.RefusedHeader != 4 || got.WrapIdx != 356 {
		t.Errorf("wrap: errno %d accepted/queued %v overrun %d refused %d write index %d (want 0, [196 256], 4, 4, 356)", got.WrapErr, got.Wrap, got.Overrun, got.RefusedHeader, got.WrapIdx)
	}
	want := []float64{0, 0, 0, 1, -1, 0.5, 1, -1}
	for k, v := range want {
		if len(got.Sanitised) != 8 || got.Sanitised[k] != v {
			t.Errorf("sanitised %v, want %v", got.Sanitised, want)
			break
		}
	}
	if got.TooLarge != 4 || got.FaultOut != 9 || got.FaultPtr != 9 || got.StatsFault != 9 {
		t.Errorf("errnos: too large %d (4), out of memory out %d (9), samples out of memory %d (9), stats out %d (9)", got.TooLarge, got.FaultOut, got.FaultPtr, got.StatsFault)
	}
	if got.Stats[0] != 0 || got.Stats[1] != 100 || got.Stats[2] != 0 || got.Stats[3] != 4 || got.Stats[4] != 1 {
		t.Errorf("stats: %v (want errno 0, played 100, underruns 0, refused 4, state playing 1)", got.Stats)
	}
	if got.ClosedErr != 0 || got.Closed != [2]int{0, 0} || got.ClosedState != 2 {
		t.Errorf("ring closed by the page: errno %d accepted/queued %v state %d (want 0, [0 0], 2)", got.ClosedErr, got.Closed, got.ClosedState)
	}
	if got.Stale != [2]int{8, 0} {
		t.Errorf("a heartbeat that stood still: accepted/queued %v (want [8 0]: waiting)", got.Stale)
	}
	if got.AfterCloseGoesToEngine != 1 {
		t.Errorf("a write on a closed stream reached the engine %d times (want 1)", got.AfterCloseGoesToEngine)
	}
}

// TestAudioStreamTheEngineDidNotAdoptStaysTheEngines: when the provider allocated
// its own ring (it did not mark the header), the worker must not write into its
// own buffer, which nothing plays: every call goes to the engine.
func TestAudioStreamTheEngineDidNotAdoptStaysTheEngines(t *testing.T) {
	b := runAudioWorker(t, "noadopt")
	var got struct {
		Open            int
		CallsDuringFeed []int
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got.CallsDuringFeed[2] < 20 {
		t.Errorf("only %d stream_write calls reached the engine for an unadopted ring (want all 20+)", got.CallsDuringFeed[2])
	}
}
