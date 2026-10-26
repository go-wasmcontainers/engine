// netprobe is its own module: its dependency (stealthrocket/net, the
// WasmEdge socket calls a user's image would make) is not the engine's.
module github.com/go-wasmcontainers/engine/testdata/wasm/netprobe

go 1.27.3

require github.com/stealthrocket/net v0.2.1
