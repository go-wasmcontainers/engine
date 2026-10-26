// Container worker of the browser executor (runtime/browser).
//
// One worker per container process. It instantiates the guest module the
// engine compiled (with a capped, shared memory), and serves every
// wasi_snapshot_preview1 import by asking the engine worker, which holds
// the container's filesystem and runs the engine's WASI host (package
// wasi): the arguments go into a control SharedArrayBuffer, a message
// wakes the engine, and this worker blocks in Atomics.wait until the
// engine has written the errno back. The engine reads and writes the
// guest's memory itself, through the shared WebAssembly.Memory this
// worker posts before the guest starts.
//
// Control block (Int32Array over ctl): [0] state 0 idle / 1 request /
// 2 done, [1] function index, [2] errno, [3] flags (1 = proc_exit),
// [4] sleep in microseconds, [5] exit code, [6] always 0 (sleep target);
// BigInt64Array at byte 64: up to 16 arguments.
"use strict";

class Exit {
  constructor(code) {
    this.code = code;
  }
}

let i32, i64;

function call(idx, noResult, args) {
  for (let i = 0; i < args.length; i++) {
    const a = args[i];
    i64[i] = typeof a === "bigint" ? a : BigInt(a >>> 0);
  }
  i32[1] = idx;
  Atomics.store(i32, 0, 1);
  postMessage({ t: "call" });
  while (Atomics.load(i32, 0) === 1) {
    Atomics.wait(i32, 0, 1);
  }
  const errno = i32[2];
  const flags = i32[3];
  const sleepMicros = i32[4];
  const code = i32[5];
  Atomics.store(i32, 0, 0);
  if (flags & 1) {
    throw new Exit(code);
  }
  if (sleepMicros > 0) {
    Atomics.wait(i32, 6, 0, sleepMicros / 1000);
  }
  return noResult ? undefined : errno;
}

onmessage = async (e) => {
  const { module, ctl, funcs } = e.data;
  i32 = new Int32Array(ctl);
  i64 = new BigInt64Array(ctl, 64, 16);
  const wasi = {};
  funcs.forEach(([name, noResult], idx) => {
    wasi[name] = (...args) => call(idx, noResult, args);
  });
  let instance;
  try {
    instance = await WebAssembly.instantiate(module, { wasi_snapshot_preview1: wasi });
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
