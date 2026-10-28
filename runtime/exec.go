package runtime

import (
	"context"
	"errors"
	"io"

	"github.com/go-wasmcontainers/engine/wasi"
)

// ExecSpec is a further process to start in a running container (`exec`):
// it runs on the container's trees, network and capabilities, in its
// process group (counted against its limits, killed with it).
type ExecSpec struct {
	// Path is the module's absolute path in the container's root.
	Path string
	// Args is argv, Args[0] included; Env is "KEY=value" entries.
	Args, Env []string
	// Stdin, Stdout and Stderr default to empty / discard.
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Terminal, when set, is the process's terminal (exec -t).
	Terminal *wasi.Terminal
}

// Execer is implemented by a Process that can start more processes in
// its container. Exec returns the new process and its pid in the
// container's group.
type Execer interface {
	Exec(ctx context.Context, spec ExecSpec) (Process, uint32, error)
}

// ErrNoExec is what Exec answers when the container's process cannot
// host another one (an executor without the engine's WASI host, or a
// process that has ended).
var ErrNoExec = errors.New("cannot start a process in this container")
