//go:build js

package browser

import (
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
)

// spawner starts a container's child processes, each in a new worker on
// the same bridge as the first: same memory limit, trees, output,
// network endpoint and capabilities. The container's group kills them
// all when its first process ends or is stopped.
type spawner struct {
	e       *Executor
	pages   uint32
	spec    runtime.Spec
	imports []wasi.Import
	group   *wasi.Group
	mem     int64
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
	wasm, e := parent.ReadModule(ss.Path)
	if e != 0 {
		s.group.Cancel(s.mem)
		return fail(e)
	}
	mod, err := s.e.compile(s.pages, wasm)
	if err != nil {
		s.group.Cancel(s.mem)
		return fail(wasi.ENOEXEC)
	}
	mounts := make([]wasi.Mount, len(s.spec.Mounts))
	for i, mt := range s.spec.Mounts {
		mounts[i] = wasi.Mount{Path: mt.Path, FS: mt.FS, ReadOnly: mt.ReadOnly}
	}
	proc := wasi.New(wasi.Config{
		Args: ss.Argv, Env: ss.Env, FS: s.spec.FS, Mounts: mounts,
		Stdin: s.spec.Stdin, Stdout: s.spec.Stdout, Stderr: s.spec.Stderr, Net: s.spec.Net,
		Modules: s.e.opt.HostModules, Caps: s.spec.Caps, Parent: parent, FDs: ss.FDs,
		Group: s.group, Spawner: s, Terminal: s.spec.Terminal, MemoryLimitBytes: s.mem, Admitted: true,
	})
	var child *wasi.Child
	p := s.e.run(mod, proc, s.imports, int64(s.pages)*65536, func(r runtime.Result) {
		child.Exit(r.ExitStatus(), parent)
	})
	child = wasi.NewChild(proc.Pid(), p.Kill)
	s.group.OnKill(proc.Pid(), p.Kill)
	return child, 0
}
