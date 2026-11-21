package browser

import "github.com/go-wasmcontainers/engine/wasi"

// The control block shared with worker.js (a SharedArrayBuffer of ctlSize
// bytes). Int32 indices first, then byte offsets; worker.js mirrors these
// values and TestWorkerMirrorsControlBlock checks it does.
const (
	ctlState   = 0 // 0 idle, 1 request, 2 done
	ctlFunc    = 1 // import index (wasi.Imports order)
	ctlErrno   = 2 // wasi_snapshot_preview1 errno
	ctlFlags   = 3 // flagExit | flagRealloc
	ctlSleep   = 4 // sleep in microseconds after the call
	ctlCode    = 5 // exit code
	ctlTarget  = 6 // always 0: the sleep's wait target
	ctlAlign   = 7 // realloc request: alignment
	ctlSizeArg = 8 // realloc request: size
	ctlPtr     = 9 // realloc answer: the pointer cabi_realloc returned

	ctlArgs = 64                         // byte offset of the 64-bit arguments
	ctlRet  = ctlArgs + 8*wasi.MaxParams // byte offset of the 64-bit result
	// Three 64-bit floats after the result, in milliseconds on the epoch clock
	// (performance.timeOrigin + performance.now(), the same clock in every
	// worker): when the guest worker posted the call, when the engine began to
	// serve it, and when the engine stored its answer. With the time the guest
	// woke they split a call into the wait for the engine's thread, the
	// execution, and the return trip (worker.js reports the slow ones).
	ctlSent   = ctlRet + 8  // byte offset: the guest worker posted the call
	ctlServed = ctlSent + 8 // the engine began to serve it
	ctlAnswer = ctlServed + 8
	ctlSize   = 256

	stateDone = 2

	flagExit    = 1
	flagRealloc = 2
)

// The layout must hold every argument and the result.
var _ [ctlSize - ctlAnswer - 8]struct{}

// slowCall is the time from posting a call to waking (ms) from which worker.js
// reports it to the engine (a "slow" message, kept in self.__hostSlow), and
// slowCap the most it reports per worker.
const (
	slowCall = 10
	slowCap  = 400
	slowKeep = 2000 // the most the engine keeps, over every worker
)
