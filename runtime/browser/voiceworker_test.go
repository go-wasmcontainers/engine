package browser

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/wasmdesk/wasmbox/audioring"
)

// voiceWorkerHarness loads worker.js into a bare context with a stub engine, and
// the REAL wasmbox-voices worklet into a second one, and drives the voice calls
// as a guest would. The stub engine answers voice_open at once (it adopts the
// buffers the worker posted, as the browser provider does) and STALLS 150 ms on
// every other call, as the real engine's thread does behind a garbage collection:
// a sample_load, voice_commit or voice_stats that reaches it is a call that
// waited. The page is played by the harness: it calls the processor's process()
// for as many 128-frame quanta as the wall time it simulates.
const voiceWorkerHarness = `
const vm = require("vm");
const fs = require("fs");
const memory = new WebAssembly.Memory({ initial: 1, maximum: 1, shared: true });
const ctlSab = new SharedArrayBuffer(4096);
const i32 = new Int32Array(ctlSab);
const cdv = new DataView(ctlSab);
const gdv = new DataView(memory.buffer);
const g8 = new Uint8Array(memory.buffer);
const FN = { fd_write: 0, voice_open: 1, sample_load: 2, voice_commit: 3, voice_stats: 4, voice_close: 5 };
const engineCalls = [0, 0, 0, 0, 0, 0];
const posted = [];
const mode = process.argv[4];
let parked = null;
function stall(ms) { const t = performance.now(); while (performance.now() - t < ms) {} }
let imports;
const sandbox = {
  postMessage: (m) => {
    posted.push(m.t);
    if (m.t === "audiovoices") { parked = m; return; }
    if (m.t !== "call") return;
    const fn = i32[1];
    engineCalls[fn]++;
    let ret = 0n;
    if (fn === FN.voice_open) {
      const out = Number(cdv.getBigUint64(64 + 8, true));
      gdv.setUint32(out, 9, true);
      if (mode !== "noadopt") Atomics.store(new Int32Array(parked.ctl, 0, 16), 10, 1);
    } else {
      stall(150);
      if (fn !== FN.voice_close) ret = 1n; // InvalidArgument: the engine does not know the handle
    }
    cdv.setBigUint64(200, ret, true);
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
vm.runInContext(fs.readFileSync(process.argv[2], "utf8"), vm.createContext(sandbox));

const reg = {};
const wctx = vm.createContext({
  AudioWorkletProcessor: class { constructor() { this.port = {}; } },
  registerProcessor: (n, c) => { reg[n] = c; },
  sampleRate: 48000,
});
vm.runInContext(fs.readFileSync(process.argv[3], "utf8"), wctx);

const SRC = 4096, CMD = 16384, SNAP = 32768, STAT = 40000;
const OUTS = [[new Float32Array(128), new Float32Array(128)]];
function quantum(proc) {
  proc.process([], OUTS);
  const l = OUTS[0][0], r = OUTS[0][1];
  let peak = 0;
  for (let k = 0; k < 128; k++) peak = Math.max(peak, Math.abs(l[k]), Math.abs(r[k]));
  return peak;
}
function cmd(slot, k, op, gen, id, loop, lv, rv, w3) {
  const at = CMD + 16 * k;
  gdv.setUint32(at, (op | (slot << 8) | (gen << 16)) >>> 0, true);
  gdv.setUint32(at + 4, (id | (loop << 16)) >>> 0, true);
  gdv.setUint32(at + 8, (lv | (rv << 16)) >>> 0, true);
  gdv.setUint32(at + 12, w3 >>> 0, true);
}

async function run() {
  const funcs = [
    ["wasi_snapshot_preview1", "fd_write", "iiii", "i"],
    ["wctr_audio_out", "voice_open", "ii", "i"],
    ["wctr_audio_out", "sample_load", "iiiiiiii", "i"],
    ["wctr_audio_out", "voice_commit", "iiii", "i"],
    ["wctr_audio_out", "voice_stats", "ii", "i"],
    ["wctr_audio_out", "voice_close", "i", "i"],
  ];
  await sandbox.onmessage({ data: { module: {}, ctl: ctlSab, funcs } });
  const a = imports.wctr_audio_out;
  const out = {};
  out.open = a.voice_open(11025, 0);
  const h = gdv.getUint32(0, true);
  out.handle = h;
  out.posted = posted.filter((t) => t === "audiovoices").length;
  out.sizes = [parked.ctl.byteLength, parked.bank.byteLength];
  const ctl = new Int32Array(parked.ctl);
  const callsAfterOpen = engineCalls.slice();
  if (mode === "noadopt") {
    out.noadopt = [a.sample_load(h, 0, SRC, 8, 8, 11025, 0, 8), a.voice_commit(h, CMD, 0, SNAP), a.voice_stats(h, STAT)];
    out.callsToEngine = engineCalls.map((n, i) => n - callsAfterOpen[i]);
    console.log(JSON.stringify(out));
    return;
  }
  const proc = new reg["wasmbox-voices"]({ processorOptions: { ctl: parked.ctl, bank: parked.bank, aheadFrames: 1103, mixRate: 11025 } });
  const t0 = performance.now();

  // A looping sample of constant 100 at 8 bits.
  g8.fill(100, SRC, SRC + 2000);
  out.load = a.sample_load(h, 0, SRC, 2000, 8, 11025, 0, 2000);
  cmd(0, 0, 1, 1, 0, 1, 255, 255, 0);
  out.start = a.voice_commit(h, CMD, 1, SNAP);
  out.startWrite = Atomics.load(ctl, 0);

  // The page plays 10 quanta, then the guest is held for 250 ms: no call at all,
  // 94 quanta of 128 frames at 48 kHz, every one of them must carry the loop.
  let silent = 0;
  for (let k = 0; k < 10; k++) if (quantum(proc) === 0) silent++;
  out.silentBefore = silent;
  silent = 0;
  for (let k = 0; k < 94; k++) if (quantum(proc) === 0) silent++;
  out.silentWhileHeld = silent;
  out.underruns = Atomics.load(ctl, 2);
  out.starved = Atomics.load(ctl, 7);
  out.playedFrames = Atomics.load(ctl, 3);

  // The guest wakes: its next commit returns the snapshot.
  out.snapCommit = a.voice_commit(h, CMD, 0, SNAP);
  out.snap0 = [gdv.getInt32(SNAP, true), gdv.getInt32(SNAP + 4, true)];
  out.stats = [a.voice_stats(h, STAT)];
  for (let k = 0; k < 6; k++) out.stats.push(gdv.getUint32(STAT + 4 * k, true));
  out.statsFault = a.voice_stats(h, 65536 - 20);

  // Refusals: an errno each, nothing written to the ring.
  const w0 = Atomics.load(ctl, 0);
  const bad = {
    op0: [0, 0, 0, 0, 0, 0, 0, 0], op5: [5, 0, 0, 0, 0, 0, 0, 0], slot128: [1, 128, 1, 0, 0, 255, 255, 0],
    id1024: [1, 0, 1, 1024, 0, 255, 255, 0], loop2: [1, 0, 1, 0, 2, 255, 255, 0],
    left256: [1, 0, 1, 0, 0, 256, 255, 0], right256: [1, 0, 1, 0, 0, 255, 256, 0], w3: [1, 0, 1, 0, 0, 255, 255, 1],
  };
  out.badCommands = {};
  for (const [name, c] of Object.entries(bad)) {
    cmd(c[1], 0, c[0], c[2], c[3], c[4], c[5], c[6], c[7]);
    out.badCommands[name] = a.voice_commit(h, CMD, 1, SNAP);
  }
  // A valid command after an invalid one in one batch: all or nothing.
  cmd(0, 0, 4, 1, 0, 0, 10, 10, 0);
  cmd(0, 1, 0, 0, 0, 0, 0, 0, 0);
  out.batchErr = a.voice_commit(h, CMD, 2, SNAP);
  out.tooMany = a.voice_commit(h, CMD, 1025, SNAP);
  out.faultCmds = a.voice_commit(h, 65536 - 8, 1, SNAP);
  out.faultSnap = a.voice_commit(h, CMD, 1, 65536 - 8);
  out.ringUntouched = Atomics.load(ctl, 0) === w0;

  // The ring: 1024 fit, the 1025th is QuotaExceeded and writes nothing; the page
  // drains it and the free-running index wraps without losing a command.
  for (let k = 0; k < 1000; k++) cmd(0, k % 8, 4, 1, 0, 0, 10 + (k % 200), 20, 0);
  out.fill = [];
  for (let n = 0; n < 1024; n += 8) { // 128 batches of 8
    const r = a.voice_commit(h, CMD, 8, SNAP);
    out.fill.push(r);
    if (r !== 0) break;
  }
  out.fullErr = a.voice_commit(h, CMD, 1, SNAP);
  out.fullWrite = Atomics.load(ctl, 0) - w0;
  quantum(proc);
  out.drained = Atomics.load(ctl, 1) === Atomics.load(ctl, 0);
  out.afterDrain = a.voice_commit(h, CMD, 8, SNAP);
  quantum(proc);
  out.wrapped = Atomics.load(ctl, 1) === Atomics.load(ctl, 0) && Atomics.load(ctl, 0) - w0 === 1032;
  out.badcmd = Atomics.load(ctl, 9);

  // sample_load refusals.
  const L = (id, ptr, len, bits, rate, loop, n) => a.sample_load(h, id, ptr, len, bits, rate, loop, n);
  out.loadErrs = {
    id1024: L(1024, SRC, 4, 8, 11025, 0, 4), bits12: L(1, SRC, 4, 12, 11025, 0, 4), rate: L(1, SRC, 4, 8, 22050, 0, 4),
    empty: L(1, SRC, 0, 8, 11025, 0, 0), len: L(1, SRC, 5, 8, 11025, 0, 4), len16: L(1, SRC, 4, 16, 11025, 0, 4),
    loop: L(1, SRC, 4, 8, 11025, 4, 4), reload: L(0, SRC, 4, 8, 11025, 0, 4), memory: L(1, 65536 - 2, 4, 8, 11025, 0, 4),
    wrapPtr: L(1, 0xFFFFFFFE, 4, 8, 11025, 0, 4),
  };
  out.loadNotMarked = Atomics.load(new Int32Array(parked.bank, 0, 16), 8) === 0; // entry 1 untouched
  // 16-bit, loop mid-sample, then the bank fills: 4 once the 16 MiB are used.
  out.load16 = L(2, SRC, 8, 16, 11025, 2, 4);
  let id = 3, last = 0;
  while (id < 1024 && (last = L(id, SRC, 60000, 8, 11025, 0, 60000)) === 0) id++;
  out.bankFull = [id, last];
  out.afterFull = L(1020, SRC, 4, 8, 11025, 0, 4) === 0 ? "small fits" : "small refused";

  // The page closes the mixer: commits are accepted and dropped.
  Atomics.store(ctl, 5, 1);
  const wc = Atomics.load(ctl, 0);
  out.closedCommit = a.voice_commit(h, CMD, 1, SNAP);
  out.closedNotWritten = Atomics.load(ctl, 0) === wc;
  a.voice_stats(h, STAT);
  out.closedState = gdv.getUint32(STAT + 20, true);
  Atomics.store(ctl, 5, 0);

  out.callsToEngine = engineCalls.map((n, i) => n - callsAfterOpen[i]);
  out.elapsedMs = performance.now() - t0;

  // voice_close is the engine's; afterwards the handle is no longer the worker's.
  out.close = a.voice_close(h);
  const before = engineCalls[FN.voice_commit];
  out.afterClose = a.voice_commit(h, CMD, 0, SNAP);
  out.afterCloseToEngine = engineCalls[FN.voice_commit] - before;

  // A hostile guest: 3000 random calls against the real worklet never throw, and
  // the worklet drains whatever the worker let through.
  const open2 = a.voice_open(11025, 0);
  const h2 = gdv.getUint32(0, true);
  const p2 = new reg["wasmbox-voices"]({ processorOptions: { ctl: parked.ctl, bank: parked.bank, aheadFrames: 1103, mixRate: 11025 } });
  let seed = 12345;
  const rnd = () => { seed = (Math.imul(seed, 1103515245) + 12345) >>> 0; return seed; };
  const errnos = new Set();
  let threw = 0;
  for (let k = 0; k < 3000; k++) {
    for (let b = 0; b < 16 * 4; b += 4) {
      const r = rnd();
      // Small values half the time, so commands often validate.
      gdv.setUint32(CMD + b, r & 0x40 ? r : (r & 0x0f0f) >>> 0, true);
    }
    try {
      const sel = rnd() % 5;
      if (sel === 0) errnos.add("l" + a.sample_load(h2, rnd() % 1100, SRC, rnd() % 70, rnd() % 40 === 0 ? 16 : 8, 11025, rnd() % 20, rnd() % 70));
      else if (sel === 1) errnos.add("s" + a.voice_stats(h2, rnd() % 70000));
      else errnos.add("c" + a.voice_commit(h2, CMD, rnd() % 5, SNAP));
      if (k % 7 === 0) quantum(p2);
    } catch (e) {
      threw++;
    }
  }
  quantum(p2);
  const ctl2 = new Int32Array(parked.ctl);
  out.hostile = { open: open2, threw, errnos: Array.from(errnos).sort(), drained: Atomics.load(ctl2, 1) === Atomics.load(ctl2, 0) };
  console.log(JSON.stringify(out));
}
run().catch((e) => { console.error(e); process.exit(1); });
`

func runVoiceWorker(t *testing.T, mode string) []byte {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("no node to run worker.js")
	}
	dir := t.TempDir()
	worker := filepath.Join(dir, "worker.js")
	worklet := filepath.Join(dir, "worklet.js")
	harness := filepath.Join(dir, "harness.js")
	for p, b := range map[string][]byte{worker: WorkerJS, worklet: audioring.WorkletJS, harness: []byte(voiceWorkerHarness)} {
		if err := os.WriteFile(p, b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, node, harness, worker, worklet, mode).CombinedOutput()
	if err != nil {
		t.Fatalf("node: %v\n%s", err, b)
	}
	return b
}

type voiceRun struct {
	Open, Handle, Posted int
	Sizes                [2]int
	Load, Start          int
	StartWrite           int
	SilentBefore         int
	SilentWhileHeld      int
	Underruns, Starved   int
	PlayedFrames         int
	SnapCommit           int
	Snap0                [2]int
	Stats                []int
	StatsFault           int
	BadCommands          map[string]int
	BatchErr, TooMany    int
	FaultCmds, FaultSnap int
	RingUntouched        bool
	Fill                 []int
	FullErr, FullWrite   int
	Drained, Wrapped     bool
	AfterDrain, Badcmd   int
	LoadErrs             map[string]int
	LoadNotMarked        bool
	Load16               int
	BankFull             [2]int
	AfterFull            string
	ClosedCommit         int
	ClosedNotWritten     bool
	ClosedState          int
	CallsToEngine        []int
	ElapsedMs            float64
	Close, AfterClose    int
	AfterCloseToEngine   int
	Noadopt              []int
	Hostile              struct {
		Open, Threw int
		Errnos      []string
		Drained     bool
	}
}

// TestVoicesKeepPlayingWhileTheGuestIsHeld: the guest loads a looping sample and
// starts it, then makes no call for 250 ms (the stall measured on CI runners);
// the real worklet, fed through worker.js's buffers, renders a loud quantum every
// time. Nothing a game does per frame (sample_load, voice_commit, voice_stats)
// reaches the engine, whose thread stalls 150 ms on every call it serves.
func TestVoicesKeepPlayingWhileTheGuestIsHeld(t *testing.T) {
	b := runVoiceWorker(t, "adopt")
	var got voiceRun
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if got.Open != 0 || got.Handle != 9 || got.Posted != 1 {
		t.Fatalf("open: errno %d handle %d, buffers posted %d\n%s", got.Open, got.Handle, got.Posted, b)
	}
	if got.Sizes != [2]int{audioring.VoicesSABBytes(), audioring.MaxBankBytes} {
		t.Errorf("buffer sizes %v, want %v", got.Sizes, [2]int{audioring.VoicesSABBytes(), audioring.MaxBankBytes})
	}
	if got.Load != 0 || got.Start != 0 || got.StartWrite != 1 {
		t.Errorf("load %d start %d ring write index %d (want 0 0 1)", got.Load, got.Start, got.StartWrite)
	}
	if got.SilentBefore != 0 || got.SilentWhileHeld != 0 {
		t.Errorf("silent quanta: %d before the stall, %d during 250 ms with the guest held (want 0 and 0)", got.SilentBefore, got.SilentWhileHeld)
	}
	if got.Underruns != 0 || got.Starved != 0 || got.PlayedFrames != 104*128 {
		t.Errorf("underruns %d starved %d played %d frames (want 0 0 %d)", got.Underruns, got.Starved, got.PlayedFrames, 104*128)
	}
	if got.SnapCommit != 0 || got.Snap0 != [2]int{1 | 1<<16, got.Snap0[1]} || got.Snap0[1] <= 0 {
		t.Errorf("snapshot of slot 0 after the stall: %v (want generation 1, active, frames remaining > 0)", got.Snap0)
	}
	if len(got.Stats) != 7 || got.Stats[0] != 0 || got.Stats[1] != 104*128 || got.Stats[2] != 0 || got.Stats[3] != 0 || got.Stats[4] <= 0 || got.Stats[5] != 0 || got.Stats[6] != 0 {
		t.Errorf("voice_stats %v (want errno 0, played %d, 0 underruns, 0 starved, painted > 0, 0 bad commands, state 0)", got.Stats, 104*128)
	}
	if got.StatsFault != 9 {
		t.Errorf("voice_stats with the slot outside memory: %d (want 9)", got.StatsFault)
	}
	if n := got.CallsToEngine[2] + got.CallsToEngine[3] + got.CallsToEngine[4]; n != 0 {
		t.Errorf("%d sample_load/voice_commit/voice_stats calls reached the engine (want 0): the game's audio waited behind its thread", n)
	}
	if got.ElapsedMs > 3000 {
		t.Errorf("the run took %.0f ms with a stalling engine: calls waited", got.ElapsedMs)
	}
}

// TestVoiceCallsRefuseWhatTheWorkletWouldRefuse: every value a guest hands in is
// checked before it reaches a shared buffer, a refusal is an errno that writes
// nothing, and a batch is all or nothing.
func TestVoiceCallsRefuseWhatTheWorkletWouldRefuse(t *testing.T) {
	b := runVoiceWorker(t, "adopt")
	var got voiceRun
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	for name, e := range got.BadCommands {
		if e != 1 {
			t.Errorf("command %s: errno %d (want 1, InvalidArgument)", name, e)
		}
	}
	if len(got.BadCommands) != 8 {
		t.Errorf("%d bad commands tried (want 8)", len(got.BadCommands))
	}
	if got.BatchErr != 1 || got.TooMany != 4 || got.FaultCmds != 9 || got.FaultSnap != 9 || !got.RingUntouched {
		t.Errorf("batch with one bad command %d (1), 1025 commands %d (4), commands outside memory %d (9), snapshot outside memory %d (9), ring untouched %v (true)",
			got.BatchErr, got.TooMany, got.FaultCmds, got.FaultSnap, got.RingUntouched)
	}
	if len(got.Fill) != 128 || got.FullErr != 5 || got.FullWrite != 1024 || !got.Drained || got.AfterDrain != 0 || !got.Wrapped {
		t.Errorf("ring: batches %v, 1025th command %d (want 5), written %d (want 1024), drained %v, commit after drain %d, wrapped %v",
			got.Fill, got.FullErr, got.FullWrite, got.Drained, got.AfterDrain, got.Wrapped)
	}
	for _, e := range got.Fill {
		if e != 0 {
			t.Errorf("a commit that fit was refused: %v", got.Fill)
			break
		}
	}
	if got.Badcmd != 0 {
		t.Errorf("the worklet refused %d commands the worker let through (want 0)", got.Badcmd)
	}
	want := map[string]int{"id1024": 1, "bits12": 1, "rate": 1, "empty": 1, "len": 1, "len16": 1, "loop": 1, "reload": 1, "memory": 9, "wrapPtr": 9}
	for name, e := range want {
		if got.LoadErrs[name] != e {
			t.Errorf("sample_load %s: errno %d (want %d)", name, got.LoadErrs[name], e)
		}
	}
	if !got.LoadNotMarked || got.Load16 != 0 {
		t.Errorf("a refused load marked its entry ready: %v; 16-bit load with a loop start errno %d (want 0)", !got.LoadNotMarked, got.Load16)
	}
	if got.BankFull[1] != 4 || got.BankFull[0] < 250 {
		t.Errorf("bank filling: stopped at id %d with errno %d (want TooLarge 4 after hundreds of samples)", got.BankFull[0], got.BankFull[1])
	}
	if got.ClosedCommit != 0 || !got.ClosedNotWritten || got.ClosedState != 1 {
		t.Errorf("a commit on a mixer the page closed: errno %d, left the ring alone %v, state %d (want 0, true, 1)", got.ClosedCommit, got.ClosedNotWritten, got.ClosedState)
	}
	if got.Close != 0 || got.AfterClose != 1 || got.AfterCloseToEngine != 1 {
		t.Errorf("after voice_close: close errno %d, later commit %d reached the engine %d times (want 0, 1, 1)", got.Close, got.AfterClose, got.AfterCloseToEngine)
	}
	if got.Hostile.Threw != 0 || !got.Hostile.Drained {
		t.Errorf("3000 random calls: %d threw, worklet drained the ring %v\n%v", got.Hostile.Threw, got.Hostile.Drained, got.Hostile.Errnos)
	}
	for _, e := range got.Hostile.Errnos {
		switch e {
		case "l0", "l1", "l4", "l9", "s0", "s9", "c0", "c1", "c4", "c5", "c9":
		default:
			t.Errorf("random call answered %s, not a documented errno", e)
		}
	}
}

// TestVoicesTheEngineDidNotAdoptStayTheEngines: when the provider did not take the
// buffers (it marked nothing), the worker must not write into buffers nothing
// plays: every call goes to the engine.
func TestVoicesTheEngineDidNotAdoptStayTheEngines(t *testing.T) {
	b := runVoiceWorker(t, "noadopt")
	var got voiceRun
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	if len(got.CallsToEngine) != 6 || got.CallsToEngine[2] != 1 || got.CallsToEngine[3] != 1 || got.CallsToEngine[4] != 1 {
		t.Errorf("calls that reached the engine: %v (want one each of sample_load, voice_commit, voice_stats)", got.CallsToEngine)
	}
}
