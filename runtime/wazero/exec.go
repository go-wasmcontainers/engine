package wazero

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/experimental"

	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
)

var _ runtime.Execer = (*process)(nil)

// Exec implements runtime.Execer: a new process of the container (exec),
// in the runtime of its first one, on its trees, network and
// capabilities, admitted into its process group (so counted against its
// limits and killed with it), with its own stdio and terminal. Its
// children inherit those. The container does not end when it does.
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
	admitted := true
	defer func() {
		if admitted {
			base.group.Cancel(base.mem)
		}
	}()
	_, compiled, err := base.e.compile(ctx, base.pages, wasm)
	if err != nil {
		return nil, 0, fmt.Errorf("%s: %w", es.Path, err)
	}
	if base.ctx.Err() != nil {
		return nil, 0, runtime.ErrNoExec
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
	// The exec's own children see its stdio and terminal, not the
	// container's first process's.
	sp := *base
	sp.spec.Stdin, sp.spec.Stdout, sp.spec.Stderr, sp.spec.Terminal = stdin, stdout, stderr, es.Terminal
	proc := wasi.New(wasi.Config{
		Args: es.Args, Env: es.Env, FS: base.spec.FS, Mounts: WASIMounts(base.spec.Mounts),
		Stdin: stdin, Stdout: stdout, Stderr: stderr, Net: base.spec.Net,
		Modules: base.e.opt.HostModules, Caps: base.spec.Caps,
		Group: base.group, Spawner: &sp, Terminal: es.Terminal, MemoryLimitBytes: base.mem, Admitted: true,
	})
	admitted = false
	runCtx, cancel := context.WithCancel(base.ctx)
	base.group.OnKill(proc.Pid(), cancel)
	q := &process{done: make(chan struct{}), cancel: cancel, limit: base.mem, exec: &sp}
	runCtx = experimental.WithMemoryAllocator(withProc(runCtx, proc), allocator(&q.mem))
	cfg := wazero.NewModuleConfig().WithName("").WithSysWalltime().WithSysNanotime().WithSysNanosleep()
	stop := context.AfterFunc(ctx, q.Kill)
	go func() {
		defer close(q.done)
		defer stop()
		defer cancel()
		mod, err := base.r.InstantiateModule(runCtx, compiled, cfg)
		if mod != nil {
			mod.Close(context.Background())
		}
		proc.Close()
		q.mu.Lock()
		killed := q.killed || base.ctx.Err() != nil
		q.mu.Unlock()
		q.res = result(err, killed, false)
	}()
	return q, proc.Pid(), nil
}
