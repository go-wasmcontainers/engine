// Container worker of the browser executor (runtime/browser).
//
// One worker per container process. It instantiates the guest module the
// engine compiled (with a capped, shared memory), and serves every import
// (wasi_snapshot_preview1 and the engine's host modules: wasi-nn,
// wasi:webgpu, ...) by asking the engine worker, which holds the
// container's filesystem and runs the engine's WASI host (package wasi):
// the arguments go into a control SharedArrayBuffer, a message wakes the
// engine, and this worker blocks in Atomics.wait until the engine has
// written the answer back. The engine reads and writes the guest's memory
// itself, through the shared WebAssembly.Memory this worker posts before
// the guest starts.
//
// A host function may first ask for memory in the guest (the canonical
// ABI returns lists and strings that way): the answer then carries
// FLAG_REALLOC, this worker calls the guest's cabi_realloc export, posts
// the pointer back and blocks again for the real answer.
//
// Control block (ctl.go is the reference; a Go test checks these values):
// Int32Array indices CTL_*; 64-bit arguments at byte CTL_ARGS, the 64-bit
// result at byte CTL_RET.
"use strict";

const CTL_STATE = 0;
const CTL_FUNC = 1;
const CTL_ERRNO = 2;
const CTL_FLAGS = 3;
const CTL_SLEEP = 4;
const CTL_CODE = 5;
const CTL_TARGET = 6;
const CTL_ALIGN = 7;
const CTL_SIZE_ARG = 8;
const CTL_PTR = 9;
const CTL_ARGS = 64;
const CTL_RET = 200;
const CTL_SENT = 208;
const CTL_SERVED = 216;
const CTL_ANSWER = 224;
const SLOW_CALL = 10;
const SLOW_CAP = 400;
const STATE_DONE = 2;
const FLAG_EXIT = 1;
const FLAG_REALLOC = 2;

class Exit {
  constructor(code) {
    this.code = code;
  }
}

let i32, view, instance;
const callNames = [];
let slowSent = 0;
let minThere = Infinity; // least served - sent over the calls so far
let minBack = Infinity; // least woke - answered

// What the guest worker did between the engine's answer and its own return
// (worker.js's "ret" in a slow call): it sat in Atomics.wait until the engine's
// notify woke it (wokeAt is the instant Atomics.wait came back with the answer,
// so answered -> wokeAt is the browser's wake-up and wokeAt -> woke is this
// worker's own code: the realloc loop and reading the ctl block), after wakes
// Atomics.wait returned from without an answer.
let wokeAt = 0;
let wakes = 0;
let reallocs = 0;

function block() {
  while (Atomics.load(i32, CTL_STATE) === 1) {
    Atomics.wait(i32, CTL_STATE, 1);
    wakes++;
  }
  wokeAt = performance.timeOrigin + performance.now();
}

function call(idx, params, result, args) {
  for (let i = 0; i < args.length; i++) {
    const off = CTL_ARGS + 8 * i;
    const a = args[i];
    switch (params[i]) {
      case "i":
        view.setBigUint64(off, BigInt(a >>> 0), true);
        break;
      case "I":
        view.setBigInt64(off, a, true);
        break;
      case "f":
        view.setBigUint64(off, 0n, true);
        view.setFloat32(off, a, true);
        break;
      case "F":
        view.setFloat64(off, a, true);
        break;
    }
  }
  i32[CTL_FUNC] = idx;
  wakes = 0;
  reallocs = 0;
  const sent = performance.timeOrigin + performance.now();
  view.setFloat64(CTL_SENT, sent, true);
  Atomics.store(i32, CTL_STATE, 1);
  postMessage({ t: "call" });
  block();
  while (i32[CTL_FLAGS] & FLAG_REALLOC) {
    reallocs++;
    const realloc = instance.exports.cabi_realloc;
    let msg = { t: "realloc" };
    if (typeof realloc !== "function") {
      msg.error = "host module needs the guest's cabi_realloc export, which it lacks";
    } else {
      i32[CTL_PTR] = realloc(0, 0, i32[CTL_ALIGN], i32[CTL_SIZE_ARG]);
    }
    i32[CTL_FLAGS] = 0;
    Atomics.store(i32, CTL_STATE, 1);
    postMessage(msg);
    block();
  }
  const errno = i32[CTL_ERRNO];
  const flags = i32[CTL_FLAGS];
  const sleepMicros = i32[CTL_SLEEP];
  const code = i32[CTL_CODE];
  Atomics.store(i32, CTL_STATE, 0);
  // Where the time of the call went (no allocation unless it was slow): the wait
  // for the engine's thread to begin it, its execution, the return trip.
  const woke = performance.timeOrigin + performance.now();
  const served = view.getFloat64(CTL_SERVED, true);
  const answered = view.getFloat64(CTL_ANSWER, true);
  // The two workers' epoch clocks differ by a constant (performance.timeOrigin
  // is a wall-clock reading taken when each worker was created, performance.now()
  // counts from it on the monotonic clock: the two readings are not taken at one
  // instant, so each worker's epoch is off by its own few ms: 3.7 ms measured).
  // The offset d (this worker's clock minus the engine's) is estimated as in
  // NTP: the trip there (served - sent = wait - d) and the trip back
  // (woke - answered = ret + d) are each smallest when the engine was idle, and
  // those two least times are equal, so d is half their difference.
  if (served - sent < minThere) {
    minThere = served - sent;
  }
  if (woke - answered < minBack) {
    minBack = woke - answered;
  }
  if (woke - sent >= SLOW_CALL && slowSent < SLOW_CAP) {
    slowSent++;
    const d = (minBack - minThere) / 2;
    // Every figure is on the ENGINE's clock, the one its busy log (busy.js) uses.
    postMessage({ t: "slow", name: callNames[idx], at: sent - d, total: woke - sent, wait: served - sent + d, exec: answered - served, ret: woke - answered - d, off: d, wake: wokeAt - answered - d, post: woke - wokeAt, wakes: wakes, reallocs: reallocs });
  }
  if (flags & FLAG_EXIT) {
    throw new Exit(code);
  }
  if (sleepMicros > 0) {
    Atomics.wait(i32, CTL_TARGET, 0, sleepMicros / 1000);
  }
  switch (result) {
    case "":
      return undefined;
    case "i":
      // wasi_snapshot_preview1 answers with its errno; host modules with
      // their result (the engine fills CTL_RET for those).
      return idx < previewCount ? errno : view.getInt32(CTL_RET, true);
    case "I":
      return view.getBigInt64(CTL_RET, true);
    case "f":
      return view.getFloat32(CTL_RET, true);
    case "F":
      return view.getFloat64(CTL_RET, true);
  }
}

// localPoll answers a poll_oneoff made only of relative clock subscriptions
// (what a guest's sleep is: Go's usleep, C's nanosleep) without asking the
// engine. The engine's reply to such a poll is fixed -- it writes the events
// of the earliest clocks, then leaves the sleep to this worker -- but it comes
// only once the engine's thread is free, so a guest that sleeps 2 ms between
// two checks waited out whatever the engine was drawing: a sleep of 2 ms took
// 20 to 66 ms behind a 46 to 107 ms draw. It returns null (and the engine
// serves the call) for anything else: a descriptor, an absolute deadline, an
// unknown clock, a bad pointer, so that every error stays the engine's.
let guestMemory;
function localPoll(inp, out, n, resultPtr) {
  inp >>>= 0;
  out >>>= 0;
  n >>>= 0;
  resultPtr >>>= 0;
  if (!guestMemory || n < 1 || n > 1024) {
    return null;
  }
  const buf = guestMemory.buffer;
  if (inp + 48 * n > buf.byteLength || out + 32 * n > buf.byteLength || resultPtr + 4 > buf.byteLength) {
    return null;
  }
  const dv = new DataView(buf);
  const timeouts = [];
  let min = null;
  for (let i = 0; i < n; i++) {
    const s = inp + 48 * i;
    if (dv.getUint8(s + 8) !== 0 || dv.getUint32(s + 16, true) > 3 || dv.getUint16(s + 40, true) !== 0) {
      return null;
    }
    let t = dv.getBigUint64(s + 24, true);
    if (t > 0x7fffffffffffffffn) {
      t = 0x7fffffffffffffffn;
    }
    timeouts.push(t);
    if (min === null || t < min) {
      min = t;
    }
  }
  let events = 0;
  for (let i = 0; i < n; i++) {
    if (timeouts[i] > min) {
      continue;
    }
    const o = out + 32 * events++;
    new Uint8Array(buf, o, 32).fill(0);
    dv.setBigUint64(o, dv.getBigUint64(inp + 48 * i, true), true);
  }
  dv.setUint32(resultPtr, events, true);
  // Microseconds, as the engine's reply rounds them, up to its 32-bit field.
  const micros = Number(min / 1000n);
  if (micros > 0) {
    Atomics.wait(i32, CTL_TARGET, 0, Math.min(micros, 0x7fffffff) / 1000);
  }
  return 0;
}

// localTrivial answers the calls that need no engine state, as the engine's
// wasi.Proc.Call does, without the round trip: a call that waited 30-60 ms for
// the engine's busy thread (a clock read, behind a draw or a GC pause) made a
// guest's audio feed late. It returns undefined when the engine must serve it.
//
//   clock_time_get(id, precision, out): id 0 is the wall clock, in ns, from
//   Date.now() (the engine's Go runtime reads the same, to the millisecond);
//   ids 1, 2, 3 are time since the process began (procStart, the engine's own
//   start stamp, on this worker's epoch clock), never going backwards; any
//   other id is EINVAL (28); an out pointer past the memory is EFAULT (21).
//   clock_res_get(id, out): 1000 ns for every id, as the engine says.
//   sched_yield(): nothing to do, 0.
//
// procStart is set by the "origin" field of the first message; without it the
// monotonic clocks are left to the engine (so that every clock of a process
// reads one origin).
let procStart = null;
let lastMono = 0;
function localTrivial(name, args) {
  if (!guestMemory) {
    return undefined;
  }
  switch (name) {
    case "sched_yield":
      return 0;
    case "clock_res_get": {
      const out = args[1] >>> 0;
      if (out + 8 > guestMemory.buffer.byteLength) {
        return 21;
      }
      new DataView(guestMemory.buffer).setBigUint64(out, 1000n, true);
      return 0;
    }
    case "clock_time_get": {
      const id = args[0] >>> 0;
      const out = args[2] >>> 0;
      let ns;
      if (id === 0) {
        ns = BigInt(Date.now()) * 1000000n;
      } else if (id >= 1 && id <= 3) {
        if (procStart === null) {
          return undefined;
        }
        let v = Math.max(0, Math.floor((performance.timeOrigin + performance.now() - procStart) * 1e6));
        if (v < lastMono) {
          v = lastMono;
        }
        lastMono = v;
        ns = BigInt(v);
      } else {
        return 28;
      }
      if (out + 8 > guestMemory.buffer.byteLength) {
        return 21;
      }
      new DataView(guestMemory.buffer).setBigUint64(out, ns, true);
      return 0;
    }
  }
  return undefined;
}

// The audio path. wctr_audio_out's stream_write and stream_stats are served
// here, on the guest's own thread, for a stream whose ring this worker
// allocated, so that a game's audio feed never queues behind the engine's thread:
// that thread also runs Go's garbage collector and serves every other host call
// of every container (slices of 60-143 ms, loop stalls up to 649 ms, measured on
// a loaded CI runner: the feed ran late and the page's worklet starved).
//
// The ring cannot be handed to this worker after stream_open (a worker blocked in
// Atomics.wait runs no event loop, so no message reaches it), so stream_open
// allocates the SharedArrayBuffer here, posts it to the engine just ahead of the
// call ("audioring": messages keep their order, so it is parked before the call
// is served), and the engine's provider adopts it, marking header word 8. A
// stream the engine did not adopt (the call failed, or the provider allocated its
// own) stays the engine's: its calls go through call().
//
// The semantics are audio/audio.go's, call for call: the same errnos in the same
// order, the same waiting / playing / closed states (a heartbeat that stood still
// for 200 ms is waiting), the same sanitising, refusals counted in header word 7
// (the engine reads them back: audio.OutOfBand). Header words: audioring's.
const A_WRITE = 0;
const A_READ = 1;
const A_UNDERRUN = 2;
const A_PLAYED = 3;
const A_OVERRUN = 4;
const A_STATE = 5;
const A_BEAT = 6;
const A_REFUSED = 7;
const A_ADOPTED = 8;
const A_HEADER = 64;
const HEARTBEAT_STALE = 200;
const audioRings = new Map();

function audioOpen(callOpen, args) {
  const rate = args[0] >>> 0;
  const ch = args[1] >>> 0;
  const cap = args[2] >>> 0;
  const out = args[3] >>> 0;
  if (!guestMemory || rate < 8000 || rate > 48000 || ch < 1 || ch > 2 || cap < 256 || cap > 16384 || (cap & (cap - 1)) !== 0 || out + 4 > guestMemory.buffer.byteLength) {
    return callOpen();
  }
  const sab = new SharedArrayBuffer(A_HEADER + cap * ch * 4);
  postMessage({ t: "audioring", sab });
  const r = callOpen();
  if (r !== 0) {
    return r;
  }
  const h = new DataView(guestMemory.buffer).getUint32(out, true);
  const hdr = new Int32Array(sab, 0, A_HEADER / 4);
  if (Atomics.load(hdr, A_ADOPTED) === 1) {
    audioRings.set(h, { hdr, data: new Float32Array(sab, A_HEADER), ch, cap, write: 0, lastHB: 0, lastHBAt: null, scratch: new Float32Array(cap * ch) });
  }
  return r;
}

// audioState is audio.go's stream.state: 2 closed, 1 playing while the
// heartbeat moves, 0 waiting.
function audioState(s, now) {
  if (Atomics.load(s.hdr, A_STATE) !== 0) {
    return 2;
  }
  const hb = Atomics.load(s.hdr, A_BEAT);
  if (hb !== s.lastHB) {
    s.lastHB = hb;
    s.lastHBAt = now;
  }
  return s.lastHBAt === null || now - s.lastHBAt > HEARTBEAT_STALE ? 0 : 1;
}

function audioWrite(s, ptr, frames, out) {
  if (frames > s.cap) {
    return 4;
  }
  const mem = guestMemory.buffer;
  if (out + 8 > mem.byteLength) {
    return 9;
  }
  const now = performance.now();
  const state = audioState(s, now);
  let accepted = 0;
  let queued = 0;
  if (frames > 0) {
    const n = frames * s.ch * 4;
    if (ptr + n > mem.byteLength) {
      return 9;
    }
    if (state === 1) {
      const pcm = s.scratch.subarray(0, frames * s.ch);
      new Uint8Array(pcm.buffer, 0, n).set(new Uint8Array(mem, ptr, n));
      for (let k = 0; k < pcm.length; k++) {
        const v = pcm[k];
        if (v !== v || v === Infinity || v === -Infinity) {
          pcm[k] = 0;
        } else if (v > 1) {
          pcm[k] = 1;
        } else if (v < -1) {
          pcm[k] = -1;
        }
      }
      accepted = audioRingWrite(s, pcm, frames);
      if (accepted < frames) {
        Atomics.add(s.hdr, A_REFUSED, frames - accepted);
      }
    } else if (state === 0) {
      accepted = frames; // never played late: accepted and dropped
    } else {
      Atomics.add(s.hdr, A_REFUSED, frames);
    }
  }
  if (state === 1) {
    queued = (s.write - Atomics.load(s.hdr, A_READ)) >>> 0;
  }
  const dv = new DataView(mem);
  dv.setUint32(out, accepted, true);
  dv.setUint32(out + 4, queued, true);
  return 0;
}

// audioRingWrite is audioring.Writer.Write: it never blocks, drops what does not
// fit (counted as an overrun), stores the samples first and the write index last.
function audioRingWrite(s, pcm, frames) {
  if (Atomics.load(s.hdr, A_STATE) !== 0) {
    return 0;
  }
  const buffered = (s.write - Atomics.load(s.hdr, A_READ)) >>> 0;
  const free = buffered >= s.cap ? 0 : s.cap - buffered;
  let n = frames;
  if (n > free) {
    Atomics.add(s.hdr, A_OVERRUN, n - free);
    n = free;
  }
  if (n === 0) {
    return 0;
  }
  const start = (s.write & (s.cap - 1)) * s.ch;
  const first = s.cap * s.ch - start;
  const take = n * s.ch;
  if (take <= first) {
    s.data.set(pcm.subarray(0, take), start);
  } else {
    s.data.set(pcm.subarray(0, first), start);
    s.data.set(pcm.subarray(first, take), 0);
  }
  s.write = (s.write + n) >>> 0;
  Atomics.store(s.hdr, A_WRITE, s.write | 0);
  return n;
}

function audioStats(s, out) {
  const mem = guestMemory.buffer;
  if (out + 16 > mem.byteLength) {
    return 9;
  }
  const state = audioState(s, performance.now());
  const dv = new DataView(mem);
  dv.setUint32(out, Atomics.load(s.hdr, A_PLAYED) >>> 0, true);
  dv.setUint32(out + 4, Atomics.load(s.hdr, A_UNDERRUN) >>> 0, true);
  dv.setUint32(out + 8, Atomics.load(s.hdr, A_REFUSED) >>> 0, true);
  dv.setUint32(out + 12, state, true);
  return 0;
}

// audioImport wraps one wctr_audio_out import; anything it does not own goes to
// the engine through callThrough.
function audioImport(name, callThrough) {
  switch (name) {
    case "stream_open":
      return (...args) => audioOpen(() => callThrough(...args), args);
    case "stream_write":
      return (...args) => {
        const s = guestMemory && audioRings.get(args[0] >>> 0);
        return s ? audioWrite(s, args[1] >>> 0, args[2] >>> 0, args[3] >>> 0) : callThrough(...args);
      };
    case "stream_stats":
      return (...args) => {
        const s = guestMemory && audioRings.get(args[0] >>> 0);
        return s ? audioStats(s, args[1] >>> 0) : callThrough(...args);
      };
    case "stream_close":
      return (...args) => {
        try {
          return callThrough(...args);
        } finally {
          audioRings.delete(args[0] >>> 0);
        }
      };
  }
  return callThrough;
}

let previewCount = 0;

onmessage = async (e) => {
  const { module, ctl, funcs, origin } = e.data;
  if (typeof origin === "number") {
    procStart = origin;
  }
  i32 = new Int32Array(ctl);
  view = new DataView(ctl);
  const imports = {};
  funcs.forEach(([mod, name, params, result], idx) => {
    callNames[idx] = mod + " " + name;
    if (mod === "wasi_snapshot_preview1") {
      previewCount = idx + 1;
    }
    imports[mod] = imports[mod] || {};
    imports[mod][name] = (...args) => call(idx, params, result, args);
    if (mod === "wasi_snapshot_preview1" && name === "poll_oneoff") {
      imports[mod][name] = (...args) => {
        const r = localPoll(args[0], args[1], args[2], args[3]);
        return r === null ? call(idx, params, result, args) : r;
      };
    }
    if (mod === "wasi_snapshot_preview1" && (name === "clock_time_get" || name === "clock_res_get" || name === "sched_yield")) {
      imports[mod][name] = (...args) => {
        const r = localTrivial(name, args);
        return r === undefined ? call(idx, params, result, args) : r;
      };
    }
    if (mod === "wctr_audio_out") {
      imports[mod][name] = audioImport(name, (...args) => call(idx, params, result, args));
    }
  });
  try {
    instance = await WebAssembly.instantiate(module, imports);
  } catch (err) {
    postMessage({ t: "trap", message: "instantiate: " + String(err) });
    return;
  }
  const memory = instance.exports.memory;
  if (!(memory instanceof WebAssembly.Memory)) {
    postMessage({ t: "trap", message: "the module exports no memory" });
    return;
  }
  guestMemory = memory;
  postMessage({ t: "memory", memory });
  const start = instance.exports._start;
  if (typeof start !== "function") {
    postMessage({ t: "trap", message: "the module exports no _start" });
    return;
  }
  try {
    start();
    postMessage({ t: "exit", code: 0 });
  } catch (err) {
    if (err instanceof Exit) {
      postMessage({ t: "exit", code: err.code });
    } else {
      postMessage({ t: "trap", message: String(err) });
    }
  }
};
