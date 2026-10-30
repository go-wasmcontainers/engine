//go:build js

package browser

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
)

var _ runtime.Execer = (*process)(nil)

// Exec implements runtime.Execer: a new process of the container in a new
// worker on the same bridge as its first one (as spawner.Spawn does for a
// child), admitted into the container's process group, with its own stdio
// and terminal, inherited by its children. The container does not end
// when it does.
func (p *process) Exec(ctx context.Context, es runtime.ExecSpec) (runtime.Process, uint32, error) {
	base := p.exec
	if base == nil {
		return nil, 0, runtime.ErrNoExec
	}
	select {
	case <-p.done:
		return nil, 0, runtime.ErrNoExec
	default:
	}
	wasm, err := runtime.LoadModule(runtime.Spec{FS: base.spec.FS, Path: es.Path})
	if err != nil {
		return nil, 0, err
	}
	if e := base.group.Admit(base.mem); e != 0 {
		return nil, 0, fmt.Errorf("%w: the container's process limit refused it (errno %d)", runtime.ErrNoExec, e)
	}
	mod, err := base.e.compile(base.pages, wasm)
	if err != nil {
		base.group.Cancel(base.mem)
		return nil, 0, fmt.Errorf("%s: %w", es.Path, err)
	}
	stdin := es.Stdin
	if stdin == nil {
		stdin = bytes.NewReader(nil)
	}
	stdout, stderr := es.Stdout, es.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	// Never read on the event loop: one *wasi.Stdin for the exec and its
	// children, served by waits.
	stdin = wasi.NewStdin(stdin)
	sp := *base
	sp.spec.Stdin, sp.spec.Stdout, sp.spec.Stderr, sp.spec.Terminal = stdin, stdout, stderr, es.Terminal
	mounts := make([]wasi.Mount, len(base.spec.Mounts))
	for i, mt := range base.spec.Mounts {
		mounts[i] = wasi.Mount{Path: mt.Path, FS: mt.FS, ReadOnly: mt.ReadOnly}
	}
	proc := wasi.New(wasi.Config{
		Args: es.Args, Env: es.Env, FS: base.spec.FS, Mounts: mounts,
		Stdin: stdin, Stdout: stdout, Stderr: stderr, Net: base.spec.Net,
		Modules: base.e.opt.HostModules, Caps: base.spec.Caps,
		Group: base.group, Spawner: &sp, Terminal: es.Terminal, MemoryLimitBytes: base.mem, Admitted: true,
	})
	q := base.e.run(mod, proc, base.imports, base.mem, nil)
	q.exec = &sp
	base.group.OnKill(proc.Pid(), q.Kill)
	context.AfterFunc(ctx, q.Kill)
	return q, proc.Pid(), nil
}
