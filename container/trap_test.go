package container

import (
	"context"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
)

// trapExec starts the real executor's process but reports it ended in a trap
// with err: what the browser executor reports when a host module's call
// fails (a WebGPU error in the browser, say).
type trapExec struct {
	runtime.Executor
	err string
}

type trapProc struct {
	runtime.Process
	err string
}

func (p trapProc) Wait() runtime.Result {
	p.Process.Wait()
	return runtime.Result{ExitCode: runtime.ExitTrap, Error: p.err}
}

func (e trapExec) Start(ctx context.Context, s runtime.Spec) (runtime.Process, error) {
	p, err := e.Executor.Start(ctx, s)
	if err != nil {
		return nil, err
	}
	return trapProc{p, e.err}, nil
}

// TestTrapIsNamedInTheContainersOutput: a container that ends in a trap says
// why on its own stderr, before the stream closes, so every client (wctr run
// --rm, the dashboard, logs) shows the failing call and the browser's message
// instead of a bare "exit status 134". The message used to live only in
// State.Error, which a --rm container takes with it.
func TestTrapIsNamedInTheContainersOutput(t *testing.T) {
	e := wazero.New(wazero.Options{CompilationCache: testutil.CompilationCache()})
	t.Cleanup(func() { e.Close(context.Background()) })
	const msg = "wasi:webgpu: get-current-texture: webgpu: getCurrentTexture: InvalidStateError: Canvas not configured"
	m := NewManager(Options{Executor: trapExec{e, msg}})
	c, code := runToExit(t, m, CreateOptions{Name: "t", Lower: image(t), Config: Config{Entrypoint: []string{"probe"}, Cmd: []string{"exit", "0"}}})
	if code != runtime.ExitTrap {
		t.Fatalf("exit code %d, want %d", code, runtime.ExitTrap)
	}
	_, stderr := output(t, m, c.ID())
	if !strings.Contains(stderr, msg) || !strings.Contains(stderr, "trap") {
		t.Fatalf("stderr %q does not name the trap %q", stderr, msg)
	}
}
