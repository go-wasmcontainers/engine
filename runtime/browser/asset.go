// Package browser is the Executor for GOOS=js: each container process runs
// in its own Web Worker (worker.js), its wasi_snapshot_preview1 imports
// served synchronously by the engine's WASI host (package wasi) in the
// worker that runs the engine, over SharedArrayBuffer and Atomics.wait.
//
// The page must be cross-origin isolated (COOP same-origin, COEP
// require-corp) for SharedArrayBuffer and shared WebAssembly memories.
package browser

import _ "embed"

// WorkerJS is the container worker script, to be served same-origin at
// the URL given to the executor.
//
//go:embed worker.js
var WorkerJS []byte
