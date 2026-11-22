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

function block() {
  while (Atomics.load(i32, CTL_STATE) === 1) {
    Atomics.wait(i32, CTL_STATE, 1);
  }
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
  const sent = performance.timeOrigin + performance.now();
  view.setFloat64(CTL_SENT, sent, true);
  Atomics.store(i32, CTL_STATE, 1);
  postMessage({ t: "call" });
  block();
  while (i32[CTL_FLAGS] & FLAG_REALLOC) {
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
    postMessage({ t: "slow", name: callNames[idx], at: sent - d, total: woke - sent, wait: served - sent + d, exec: answered - served, ret: woke - answered - d, off: d });
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

let previewCount = 0;

onmessage = async (e) => {
  const { module, ctl, funcs } = e.data;
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
