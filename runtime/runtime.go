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
	"github.com/go-wasmcontainers/engine/wasi"
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
	// Caps are capabilities granted to the process ("gpu", from run
	// --gpu); host modules read them through the engine's WASI host.
	Caps []string
	// Terminal is the container's terminal (run -t), if any; the engine's
	// WASI host reports it to the guest's processes.
	Terminal *wasi.Terminal
	// Processes bounds the processes the container may spawn (the
	// engine's WASI host only; zero values are its defaults).
	Processes wasi.GroupLimits
}

// Result is how a process ended.
type Result struct {
	ExitCode int
	// ProcExit reports that the guest ended with proc_exit(ProcCode), the
	// raw u32; ExitCode is then bounded to 0..125 (see ExitCodeOf).
	ProcExit bool
	ProcCode uint32
	// TimedOut reports a wall-clock cancel.
	TimedOut bool
	// Killed reports a cancel through Process.Kill or the context.
	Killed bool
	// Error describes an abnormal end (a trap), empty on a normal exit.
	Error string
}

// ExitStatus is how a parent process sees this end: killed or timed out
// as signal 9, a trap as signal 6, otherwise the exit code.
func (r Result) ExitStatus() wasi.ExitStatus {
	switch {
	case r.Killed || r.TimedOut:
		return wasi.ExitStatus{Signal: 9}
	case r.ExitCode == ExitTrap && r.Error != "":
		return wasi.ExitStatus{Signal: 6}
	}
	if r.ProcExit {
		return wasi.ExitStatus{Code: r.ProcCode}
	}
	return wasi.ExitStatus{Code: uint32(r.ExitCode)}
}

// GuestExit is the Result of a guest's proc_exit(code): ExitCode is the
// bounded Docker-compatible status (ExitCodeOf), ProcCode the raw u32 a
// WASIX parent still sees.
func GuestExit(code uint32) Result {
	return Result{ExitCode: ExitCodeOf(code), ProcExit: true, ProcCode: code}
}

// Exit codes the engine uses for ends that are not a proc_exit, following
// the shell and Docker conventions (128+SIGKILL for a kill).
const (
	ExitTrap    = 134 // 128 + SIGABRT: the module trapped
	ExitKilled  = 137 // 128 + SIGKILL: killed or timed out
	ExitNoExec  = 126 // found but not runnable (not a valid module)
	ExitMissing = 127 // module not found
)

// ExitCodeOf bounds a guest's proc_exit code (a u32: os.Exit(-1) arrives as
// 4294967295) the way wasmtime does. Its preview 1 proc_exit
// (crates/wasi/src/p1.rs) fails with "exit with invalid exit status outside
// of [0..126)" for a status >= 126, and the CLI then exits 1. So 0..125 is
// kept and anything else is 1: never 4294967295, never above 255, never
// negative, whatever the Docker API or wctr relays.
func ExitCodeOf(code uint32) int {
	if code > 125 {
		return 1
	}
	return int(code)
}

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
