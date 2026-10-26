// Package runtime defines how the engine runs a container's module. The
// Executor interface carries no type of any WebAssembly runtime, so the
// native executor (runtime/wazero) and the browser executor that comes
// later implement the same contract.
package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/rootfs"
)

// Spec is everything needed to run one container process.
type Spec struct {
	// ID names the process in errors and logs.
	ID string
	// FS is the container's root filesystem, preopened as "/".
	FS *rootfs.FS
	// Mounts are volumes, each preopened at its own path.
	Mounts []Mount
	// Path is the module's absolute path inside FS.
	Path string
	// Args is argv, Args[0] included.
	Args []string
	// Env is "KEY=value" entries.
	Env []string
	// Stdin, Stdout and Stderr default to empty / discard.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// MemoryLimitBytes bounds the module's linear memory (0 = executor
	// default). It is rounded down to whole 64 KiB pages.
	MemoryLimitBytes int64
	// Timeout cancels the run after this wall-clock time (0 = none).
	Timeout time.Duration
	// Net is the container's network endpoint (nil: no network). Only
	// the engine's WASI host serves sockets.
	Net netdriver.Endpoint
}

// Result is how a process ended.
type Result struct {
	ExitCode int
	// TimedOut reports a wall-clock cancel.
	TimedOut bool
	// Killed reports a cancel through Process.Kill or the context.
	Killed bool
	// Error describes an abnormal end (a trap), empty on a normal exit.
	Error string
}

// Exit codes the engine uses for ends that are not a proc_exit, following
// the shell and Docker conventions (128+SIGKILL for a kill).
const (
	ExitTrap    = 134 // 128 + SIGABRT: the module trapped
	ExitKilled  = 137 // 128 + SIGKILL: killed or timed out
	ExitNoExec  = 126 // found but not runnable (not a valid module)
	ExitMissing = 127 // module not found
)

// Errors a Start may return; the exit code to record goes with each.
var (
	ErrNotFound = errors.New("module not found")
	ErrInvalid  = errors.New("not a valid WebAssembly module")
)

// LoadModule reads and checks the module a spec names: present (else
// ErrNotFound), a regular file with the wasm magic (else ErrInvalid).
func LoadModule(spec Spec) ([]byte, error) {
	if spec.FS == nil {
		return nil, errors.New("spec has no filesystem")
	}
	st, err := spec.FS.Stat(spec.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%s: %w", spec.Path, ErrNotFound)
		}
		return nil, fmt.Errorf("%s: %w: %v", spec.Path, ErrInvalid, err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s: %w: not a regular file", spec.Path, ErrInvalid)
	}
	wasm, err := spec.FS.ReadFile(spec.Path)
	if err != nil {
		return nil, err
	}
	if !bytes.HasPrefix(wasm, []byte("\x00asm")) {
		return nil, fmt.Errorf("%s: %w: no wasm magic", spec.Path, ErrInvalid)
	}
	return wasm, nil
}

// Mount is a tree (a volume) preopened at Path in the guest, read-only
// when ReadOnly.
type Mount struct {
	Path     string
	FS       *rootfs.FS
	ReadOnly bool
}

// Process is a started run.
type Process interface {
	// Wait blocks until the process ends.
	Wait() Result
	// Kill ends the process; Wait then reports Killed.
	Kill()
}

// Stats is what an executor can measure of a running process.
type Stats struct {
	// MemoryBytes is the guest's linear memory now (it never shrinks);
	// MemoryLimitBytes the most it may grow to.
	MemoryBytes, MemoryLimitBytes int64
}

// StatsReporter is implemented by processes that can report Stats.
type StatsReporter interface {
	Stats() Stats
}

// Executor starts processes.
type Executor interface {
	// Start validates and compiles the module, then runs it in the
	// background. A missing module wraps ErrNotFound, an invalid one
	// ErrInvalid.
	Start(ctx context.Context, spec Spec) (Process, error)
	// Close releases the executor's resources.
	Close(ctx context.Context) error
}
