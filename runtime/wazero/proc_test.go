package wazero

import (
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
)

// procModule is a minimal process module over the engine's primitives,
// the shape a WASIX module takes: pipe, spawn (stdout redirected), join
// (blocking on the child without blocking the engine), getpid.
type procModule struct{}

func (procModule) Module() string { return "wctr_proc" }
func (procModule) Funcs() []wasi.HostFunc {
	return []wasi.HostFunc{
		{Name: "pipe", Params: "i", Result: "i"},
		{Name: "spawn", Params: "iiiiii", Result: "i"},
		{Name: "join", Params: "ii", Result: "i"},
		{Name: "getpid", Result: "i"},
	}
}
func (procModule) Open(p *wasi.Proc) wasi.Instance { return &procInst{p: p} }

type procInst struct{ p *wasi.Proc }

func (*procInst) Close() {}

func ret(e uint32) wasi.Result { return wasi.Result{Ret: uint64(e)} }

func (i *procInst) Call(fn int, a []uint64, m wasi.Memory) wasi.Result {
	p := i.p
	switch fn {
	case 0:
		r, w, e := p.Pipe()
		if e != 0 {
			return ret(e)
		}
		var b [8]byte
		binary.LittleEndian.PutUint32(b[0:], r)
		binary.LittleEndian.PutUint32(b[4:], w)
		if !m.Write(uint32(a[0]), b[:]) {
			return ret(wasi.EFAULT)
		}
		return ret(0)
	case 1:
		path, ok1 := m.Read(uint32(a[0]), uint32(a[1]))
		argv, ok2 := m.Read(uint32(a[2]), uint32(a[3]))
		if !ok1 || !ok2 {
			return ret(wasi.EFAULT)
		}
		out := uint32(a[4])
		var ops []wasi.FDOp
		if out != 1 {
			ops = []wasi.FDOp{{Kind: wasi.FDDup2, FD: out, To: 1}, {Kind: wasi.FDClose, FD: out}}
		}
		t, e := p.CloneFDs("/", ops)
		if e != 0 {
			return ret(e)
		}
		args := strings.Split(strings.TrimSuffix(string(argv), "\x00"), "\x00")
		pidPtr := uint32(a[5])
		return p.SpawnWait(wasi.SpawnSpec{Path: string(path), Argv: args, FDs: t}, func(c *wasi.Child, e uint32, m wasi.Memory) wasi.Result {
			if e != 0 {
				return ret(e)
			}
			var b [4]byte
			binary.LittleEndian.PutUint32(b[:], c.Pid())
			m.Write(pidPtr, b[:])
			return ret(0)
		})
	case 2:
		return i.join(uint32(a[0]), uint32(a[1]), m)
	case 3:
		return wasi.Result{Ret: uint64(p.Pid())}
	}
	return wasi.Result{Trap: "no such function"}
}

func (i *procInst) join(pid, ptr uint32, m wasi.Memory) wasi.Result {
	wake := i.p.ChildChanged()
	c, e := i.p.Reap(pid)
	if e == wasi.EAGAIN {
		return wasi.Result{Wait: wake, Resume: func(m wasi.Memory) wasi.Result { return i.join(pid, ptr, m) }}
	}
	if e != 0 {
		return ret(e)
	}
	var b [8]byte
	binary.LittleEndian.PutUint32(b[0:], c.Status().Code)
	binary.LittleEndian.PutUint32(b[4:], uint32(c.Status().Signal))
	m.Write(ptr, b[:])
	return ret(0)
}

func procExecutor(t *testing.T) *Executor {
	o := opts()
	o.HostModules = []wasi.HostModule{procModule{}}
	e := New(o)
	t.Cleanup(func() { e.Close(context.Background()) })
	return e
}

// TestSpawn runs testdata/wasm/spawner through one executor (its compiled
// modules cached across the cases, which keeps the race-enabled CI lanes
// short):
//   - pipeline: a child's stdout on a pipe, read to end of file once the
//     child (the last writer) exits, then joined with its exit code;
//   - codes: an exit code above 255 survives; a missing module is ENOENT,
//     a file that is not a module ENOEXEC, a join with no child ECHILD;
//   - orphan: a spinning child does not keep the container alive once its
//     first process exits.
func TestSpawn(t *testing.T) {
	img := lower(t, "spawner", "hello", "probe")
	e := procExecutor(t)
	r := start(t, e, runtime.Spec{FS: img.Clone(rootfs.Quota{}), Path: "/bin/spawner.wasm", Args: []string{"spawner", "pipeline"}})
	want := fmt.Sprintf("me=1 child=2 read=%q err=<nil> code=0 sig=0 join=0\n", "Hello from a WebAssembly container!\n")
	if r.stdout != want || r.res.ExitCode != 0 {
		t.Errorf("pipeline: stdout %q (exit %d, %s, stderr %q), want %q", r.stdout, r.res.ExitCode, r.res.Error, r.stderr, want)
	}
	r = start(t, e, runtime.Spec{FS: img.Clone(rootfs.Quota{}), Path: "/bin/spawner.wasm", Args: []string{"spawner", "codes"}})
	want = fmt.Sprintf("300 0 0\n%d %d %d\n", wasi.ENOENT, wasi.ENOEXEC, wasi.ECHILD)
	if r.stdout != want {
		t.Errorf("codes: stdout %q (stderr %q, %s), want %q", r.stdout, r.stderr, r.res.Error, want)
	}
	done := make(chan run, 1)
	go func() {
		done <- start(t, e, runtime.Spec{FS: img.Clone(rootfs.Quota{}), Path: "/bin/spawner.wasm", Args: []string{"spawner", "orphan"}})
	}()
	select {
	case r := <-done:
		if r.stdout != "leaving\n" || r.res.ExitCode != 0 {
			t.Fatalf("%+v", r)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the container did not stop")
	}
}
