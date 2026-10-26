package wazero

import (
	"context"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
)

// engineWASI switches the executor tests to the engine's own WASI host.
var engineWASI bool

func opts() Options { return Options{EngineWASI: engineWASI} }

// TestEngineWASI runs the executor's guest-level tests a second time with
// the engine's WASI host (package wasi) in place of wazero's: the same
// real Go guests, the same expected output, errors and exit codes. This is
// what the browser executor runs, proven natively first.
func TestEngineWASI(t *testing.T) {
	engineWASI = true
	defer func() { engineWASI = false }()
	t.Run("programs", TestRunPrograms)
	t.Run("filesystem", TestFilesystemThroughWASI)
	t.Run("limits", TestLimits)
	t.Run("concurrent", TestConcurrentContainers)

	// What only the engine host does: ENOSPC instead of EIO.
	img := lower(t, "probe")
	e := New(opts())
	defer e.Close(context.Background())
	small := img.Clone(rootfs.Quota{MaxBytes: 2})
	r := start(t, e, runtime.Spec{FS: small, Path: "/bin/probe.wasm", Args: []string{"probe", "fs"}})
	if r.res.ExitCode != 1 || !strings.Contains(r.stderr, "No space left on device") {
		t.Fatalf("%+v", r)
	}
}
