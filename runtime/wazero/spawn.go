package wazero

import (
	"context"

	"github.com/tetratelabs/wazero"

	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
)

// spawner starts a container's child processes in the runtime of its
// first one: same memory limit, same trees, same output, the container's
// network endpoint and capabilities. A child runs under a context derived
// from the container's, so stopping the container (or its first process
// ending) stops every child.
type spawner struct {
	e     *Executor
	r     wazero.Runtime
	pages uint32
	spec  runtime.Spec // the container's first process
	ctx   context.Context
	group *wasi.Group
	mem   int64
}

func (s *spawner) Spawn(parent *wasi.Proc, ss wasi.SpawnSpec) (*wasi.Child, uint32) {
	fail := func(e uint32) (*wasi.Child, uint32) {
		if ss.FDs != nil {
			ss.FDs.Close(parent)
		}
		return nil, e
	}
	if e := s.group.Admit(s.mem); e != 0 {
		return fail(e)
	}
	admitted := true
	defer func() {
		if admitted {
			s.group.Cancel(s.mem)
		}
	}()
	wasm, e := parent.ReadModule(ss.Path)
	if e != 0 {
		return fail(e)
	}
	_, compiled, err := s.e.compile(s.ctx, s.pages, wasm)
	if err != nil {
		return fail(wasi.ENOEXEC)
	}
	if s.ctx.Err() != nil {
		return fail(wasi.ESRCH)
	}
	proc := wasi.New(wasi.Config{
		Args: ss.Argv, Env: ss.Env, FS: s.spec.FS, Mounts: WASIMounts(s.spec.Mounts),
		Stdin: s.spec.Stdin, Stdout: s.spec.Stdout, Stderr: s.spec.Stderr, Net: s.spec.Net,
		Modules: s.e.opt.HostModules, Caps: s.spec.Caps, Parent: parent, FDs: ss.FDs,
		Group: s.group, Spawner: s, Terminal: s.spec.Terminal, MemoryLimitBytes: s.mem, Admitted: true, Label: s.spec.ID,
	})
	admitted = false
	ctx, cancel := context.WithCancel(s.ctx)
	child := wasi.NewChild(proc.Pid(), cancel)
	s.group.OnKill(proc.Pid(), cancel)
	cfg := wazero.NewModuleConfig().WithName("").WithSysWalltime().WithSysNanotime().WithSysNanosleep()
	go func() {
		defer cancel()
		mod, err := s.r.InstantiateModule(withProc(ctx, proc), compiled, cfg)
		if mod != nil {
			mod.Close(context.Background())
		}
		proc.Close()
		res := result(err, ctx.Err() != nil, false)
		child.Exit(res.ExitStatus(), parent)
	}()
	return child, 0
}
