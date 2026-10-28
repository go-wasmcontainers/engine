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
const STATE_DONE = 2;
const FLAG_EXIT = 1;
const FLAG_REALLOC = 2;

class Exit {
  constructor(code) {
    this.code = code;
  }
}

let i32, view, instance;

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

let previewCount = 0;

onmessage = async (e) => {
  const { module, ctl, funcs } = e.data;
  i32 = new Int32Array(ctl);
  view = new DataView(ctl);
  const imports = {};
  funcs.forEach(([mod, name, params, result], idx) => {
    if (mod === "wasi_snapshot_preview1") {
      previewCount = idx + 1;
    }
    imports[mod] = imports[mod] || {};
    imports[mod][name] = (...args) => call(idx, params, result, args);
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
