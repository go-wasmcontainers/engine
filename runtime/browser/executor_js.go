//go:build js

package browser

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"syscall/js"
	"time"

	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
	"github.com/go-wasmcontainers/engine/wasmbin"
)

// Options configures the executor.
type Options struct {
	// WorkerURL is where worker.js (WorkerJS) is served, same origin.
	WorkerURL string
	// DefaultMemoryLimitBytes applies when a Spec sets none (256 MiB).
	DefaultMemoryLimitBytes int64
	// MaxCompiled bounds the compiled-module cache (default 32).
	MaxCompiled int
	// CodeCache names a Cache Storage cache that keeps modules across
	// page loads, under CodeCacheURL + <sha256>.wasm (same origin), so the
	// browser can reuse its compiled code; "" disables it.
	CodeCache, CodeCacheURL string
	// OnCompile, when set, is told about each module the executor got.
	OnCompile func(CompileStat)
	// HostModules are served beside wasi_snapshot_preview1 by the engine's
	// WASI host in this worker; worker.js gives the guest one import
	// object per module.
	HostModules []wasi.HostModule
}

// Executor runs each container process in a Web Worker.
type Executor struct {
	opt Options

	mu       sync.Mutex
	compiled map[cacheKey]*cacheEntry
	tick     uint64
}

type cacheKey struct {
	pages  uint32
	digest image.Digest
}

type cacheEntry struct {
	mod  js.Value
	used uint64
}

var _ runtime.Executor = (*Executor)(nil)

// New returns an executor.
func New(opt Options) *Executor {
	if opt.WorkerURL == "" {
		opt.WorkerURL = "container-worker.js"
	}
	if opt.DefaultMemoryLimitBytes <= 0 {
		opt.DefaultMemoryLimitBytes = 256 << 20
	}
	if opt.MaxCompiled <= 0 {
		opt.MaxCompiled = 32
	}
	return &Executor{opt: opt, compiled: map[cacheKey]*cacheEntry{}}
}

// Close implements runtime.Executor.
func (e *Executor) Close(context.Context) error { return nil }

// CacheLen reports how many compiled modules are cached.
func (e *Executor) CacheLen() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.compiled)
}

// await blocks the calling goroutine (never a js.Func callback) until p
// settles.
func await(p js.Value) (js.Value, error) {
	done := make(chan struct{})
	var res js.Value
	var err error
	then := js.FuncOf(func(_ js.Value, args []js.Value) any {
		res = args[0]
		close(done)
		return nil
	})
	catch := js.FuncOf(func(_ js.Value, args []js.Value) any {
		err = errors.New(args[0].Call("toString").String())
		close(done)
		return nil
	})
	p.Call("then", then, catch)
	<-done
	then.Release()
	catch.Release()
	return res, err
}

func uint8Array(b []byte) js.Value {
	u := js.Global().Get("Uint8Array").New(len(b))
	js.CopyBytesToJS(u, b)
	return u
}

func pagesFor(bytes int64) uint32 {
	p := bytes / 65536
	switch {
	case p < 1:
		return 1
	case p > wasmbin.MaxPages:
		return wasmbin.MaxPages
	}
	return uint32(p)
}

func (e *Executor) compile(pages uint32, wasm []byte) (js.Value, error) {
	key := cacheKey{pages, image.FromBytes(wasm)}
	t0 := time.Now()
	e.mu.Lock()
	e.tick++
	if c, ok := e.compiled[key]; ok {
		c.used = e.tick
		e.mu.Unlock()
		if e.opt.OnCompile != nil {
			e.opt.OnCompile(CompileStat{Source: "memory", Bytes: len(wasm), Took: time.Since(t0)})
		}
		return c.mod, nil
	}
	e.mu.Unlock()
	// The cap must be in the binary: a browser cannot bound a memory the
	// module defines. Shared, so this worker can serve WASI on it.
	patched, err := wasmbin.LimitMemory(wasm, pages, true)
	if err != nil {
		return js.Value{}, fmt.Errorf("%w: %v", runtime.ErrInvalid, err)
	}
	mod, st, err := e.compileCached(key, patched)
	if err != nil {
		return js.Value{}, fmt.Errorf("%w: %v", runtime.ErrInvalid, err)
	}
	st.Took = time.Since(t0)
	if e.opt.OnCompile != nil {
		e.opt.OnCompile(st)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.compiled) >= e.opt.MaxCompiled {
		var oldK cacheKey
		var old *cacheEntry
		for k, c := range e.compiled {
			if old == nil || c.used < old.used {
				oldK, old = k, c
			}
		}
		delete(e.compiled, oldK)
	}
	e.compiled[key] = &cacheEntry{mod: mod, used: e.tick}
	return mod, nil
}

// jsMemory is the guest's shared memory, seen from the engine worker. Its
// buffer is fetched on every access: a grow replaces it.
type jsMemory struct {
	mem js.Value
}

func (m jsMemory) view(off, n uint32) (js.Value, bool) {
	if m.mem.IsUndefined() || m.mem.IsNull() {
		return js.Value{}, false
	}
	buf := m.mem.Get("buffer")
	size := uint64(buf.Get("byteLength").Float())
	if uint64(off)+uint64(n) > size {
		return js.Value{}, false
	}
	return js.Global().Get("Uint8Array").New(buf, off, n), true
}

// size is the memory's current length in bytes.
func (m jsMemory) size() uint64 {
	if m.mem.IsUndefined() || m.mem.IsNull() {
		return 0
	}
	return uint64(m.mem.Get("buffer").Get("byteLength").Float())
}

// JSMemory returns the guest's shared WebAssembly.Memory, so a host module
// can hand a view of it to a browser API (WebGPU's writeBuffer takes a
// shared buffer) instead of copying through Go.
func (m jsMemory) JSMemory() js.Value { return m.mem }

func (m jsMemory) Read(off, n uint32) ([]byte, bool) {
	v, ok := m.view(off, n)
	if !ok {
		return nil, false
	}
	b := make([]byte, n)
	js.CopyBytesToGo(b, v)
	return b, true
}

func (m jsMemory) Write(off uint32, b []byte) bool {
	v, ok := m.view(off, uint32(len(b)))
	if !ok {
		return false
	}
	js.CopyBytesToJS(v, b)
	return true
}

type process struct {
	worker js.Value
	funcs  []js.Func
	once   sync.Once
	done   chan struct{}
	res    runtime.Result
	killed bool
	timer  *time.Timer

	mu    sync.Mutex
	mem   js.Value // the guest's shared memory, once its worker sent it
	limit int64
	proc  *wasi.Proc
	onEnd func(runtime.Result)
	// exec starts further processes in this container (exec_js.go).
	exec *spawner
}

func (p *process) Wait() runtime.Result { <-p.done; return p.res }

// Stats implements runtime.StatsReporter: the shared memory's current
// length (a grow replaces its buffer, so it is fetched each time).
func (p *process) Stats() runtime.Stats {
	p.mu.Lock()
	m := p.mem
	p.mu.Unlock()
	s := runtime.Stats{MemoryLimitBytes: p.limit}
	if !m.IsUndefined() && !m.IsNull() {
		s.MemoryBytes = int64(m.Get("buffer").Get("byteLength").Float())
	}
	return s
}

func (p *process) finish(r runtime.Result) {
	p.once.Do(func() {
		if p.timer != nil {
			p.timer.Stop()
		}
		p.worker.Call("terminate")
		p.res = r
		close(p.done)
		go func() {
			p.proc.Close() // its descriptors, sockets, host modules
			if p.onEnd != nil {
				p.onEnd(r)
			}
		}()
		// Release the callbacks after the current one returns.
		go func() {
			for _, f := range p.funcs {
				f.Release()
			}
		}()
	})
}

// Kill implements runtime.Process: the worker is terminated, which ends the
// guest wherever it is, even inside a loop with no WASI call.
func (p *process) Kill() {
	p.finish(runtime.Result{ExitCode: runtime.ExitKilled, Killed: true, Error: "killed"})
}

// Start implements runtime.Executor.
func (e *Executor) Start(ctx context.Context, spec runtime.Spec) (runtime.Process, error) {
	wasm, err := runtime.LoadModule(spec)
	if err != nil {
		return nil, err
	}
	limit := spec.MemoryLimitBytes
	if limit <= 0 {
		limit = e.opt.DefaultMemoryLimitBytes
	}
	mod, err := e.compile(pagesFor(limit), wasm)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", spec.Path, err)
	}
	args := spec.Args
	if len(args) == 0 {
		args = []string{spec.Path}
	}
	mounts := make([]wasi.Mount, len(spec.Mounts))
	for i, mt := range spec.Mounts {
		mounts[i] = wasi.Mount{Path: mt.Path, FS: mt.FS, ReadOnly: mt.ReadOnly}
	}
	imports, err := wasi.Imports(e.opt.HostModules)
	if err != nil {
		return nil, err
	}
	group := wasi.NewGroup(spec.Processes)
	mem := int64(pagesFor(limit)) * 65536
	// Stdin is read off the engine's event loop (a guest's fd_read waits
	// instead), and shared with the children.
	if spec.Stdin != nil {
		spec.Stdin = wasi.NewStdin(spec.Stdin)
	}
	sp := &spawner{e: e, pages: pagesFor(limit), spec: spec, imports: imports, group: group, mem: mem}
	proc := wasi.New(wasi.Config{Args: args, Env: spec.Env, FS: spec.FS, Mounts: mounts, Stdin: spec.Stdin, Stdout: spec.Stdout, Stderr: spec.Stderr, Net: spec.Net,
		Modules: e.opt.HostModules, Caps: spec.Caps, Group: group, Spawner: sp, Terminal: spec.Terminal, MemoryLimitBytes: mem, Label: spec.ID})
	p := e.run(mod, proc, imports, limit, func(runtime.Result) {
		group.Kill() // a container ends with its first process
	})
	p.exec = sp
	if spec.Timeout > 0 {
		p.timer = time.AfterFunc(spec.Timeout, func() {
			p.finish(runtime.Result{ExitCode: runtime.ExitKilled, TimedOut: true, Error: "wall-clock timeout"})
		})
	}
	context.AfterFunc(ctx, p.Kill)
	return p, nil
}

// run starts proc's module in a new worker; onEnd runs once it has ended
// and its descriptors are released.
func (e *Executor) run(mod js.Value, proc *wasi.Proc, imports []wasi.Import, limit int64, onEnd func(runtime.Result)) *process {
	ctl := js.Global().Get("SharedArrayBuffer").New(ctlSize)
	i32 := js.Global().Get("Int32Array").New(ctl)
	ctlU8 := js.Global().Get("Uint8Array").New(ctl, 0, ctlSize)
	atomics := js.Global().Get("Atomics")
	worker := js.Global().Get("Worker").New(e.opt.WorkerURL)
	p := &process{worker: worker, done: make(chan struct{}), mem: js.Undefined(), limit: int64(pagesFor(limit)) * 65536, proc: proc, onEnd: onEnd}
	mem := jsMemory{mem: js.Undefined()}
	ctlBuf := make([]byte, ctlSize)
	// pending is the continuation of a host call waiting for the guest's
	// cabi_realloc (the worker answers with a "realloc" message).
	var pending *wasi.Realloc
	var reply func(fn int, res wasi.Result)
	// serve finishes a call: an answer, a realloc request, or a wait for a
	// socket or a promise (never in this callback: those need the event
	// loop; the guest stays blocked in Atomics.wait meanwhile).
	var serve func(fn int, res wasi.Result)
	serve = func(fn int, res wasi.Result) {
		switch {
		case res.Trap != "":
			p.finish(runtime.Result{ExitCode: runtime.ExitTrap, Error: res.Trap})
		case res.Realloc != nil:
			pending = res.Realloc
			i32.SetIndex(ctlFlags, flagRealloc)
			i32.SetIndex(ctlAlign, int(int32(res.Realloc.Align)))
			i32.SetIndex(ctlSizeArg, int(int32(res.Realloc.Size)))
			atomics.Call("store", i32, ctlState, stateDone)
			atomics.Call("notify", i32, ctlState)
		case res.Wait == nil:
			reply(fn, res)
		default:
			go func(res wasi.Result, mem jsMemory) {
				for res.Wait != nil {
					var timeout <-chan time.Time
					if res.Sleep > 0 {
						t := time.NewTimer(res.Sleep)
						timeout = t.C
						defer t.Stop()
					}
					select {
					case <-res.Wait:
					case <-timeout:
					case <-p.done:
						return // killed: the worker is gone
					}
					res = res.Resume(mem)
				}
				serve(fn, res)
			}(res, mem)
		}
	}
	current := -1 // the import being served
	reply = func(fn int, res wasi.Result) {
		flags := 0
		if res.Exit {
			flags = flagExit
		}
		sleep := res.Sleep.Microseconds()
		if sleep > math.MaxInt32 {
			sleep = math.MaxInt32
		}
		i32.SetIndex(ctlErrno, int(res.Errno))
		i32.SetIndex(ctlFlags, flags)
		i32.SetIndex(ctlSleep, int(sleep))
		i32.SetIndex(ctlCode, int(int32(res.Code)))
		if fn >= len(wasi.Funcs) {
			var b [8]byte
			binary.LittleEndian.PutUint64(b[:], res.Ret)
			js.CopyBytesToJS(js.Global().Get("Uint8Array").New(ctl, ctlRet, 8), b[:])
		}
		atomics.Call("store", i32, ctlState, stateDone)
		atomics.Call("notify", i32, ctlState)
	}

	onMessage := js.FuncOf(func(_ js.Value, a []js.Value) any {
		data := a[0].Get("data")
		switch data.Get("t").String() {
		case "memory":
			mem = jsMemory{mem: data.Get("memory")}
			p.mu.Lock()
			p.mem = mem.mem
			p.mu.Unlock()
		case "call":
			js.CopyBytesToGo(ctlBuf, ctlU8)
			fn := int(int32(binary.LittleEndian.Uint32(ctlBuf[4*ctlFunc:])))
			if fn < 0 || fn >= len(imports) {
				p.finish(runtime.Result{ExitCode: runtime.ExitTrap, Error: fmt.Sprintf("call to unknown import %d", fn)})
				break
			}
			current = fn
			n := len(imports[fn].Params)
			callArgs := make([]uint64, n)
			for i := 0; i < n; i++ {
				callArgs[i] = binary.LittleEndian.Uint64(ctlBuf[ctlArgs+8*i:])
			}
			serve(fn, proc.CallImport(fn, callArgs, mem))
		case "realloc":
			// The worker ran the guest's cabi_realloc for the pending call.
			r := pending
			pending = nil
			if r == nil || current < 0 {
				p.finish(runtime.Result{ExitCode: runtime.ExitTrap, Error: "unexpected realloc answer"})
				break
			}
			if data.Get("error").Truthy() {
				p.finish(runtime.Result{ExitCode: runtime.ExitTrap, Error: data.Get("error").String()})
				break
			}
			js.CopyBytesToGo(ctlBuf, ctlU8)
			ptr := binary.LittleEndian.Uint32(ctlBuf[4*ctlPtr:])
			if err := wasi.CheckBlock(ptr, r.Align, r.Size, mem.size()); err != nil {
				p.finish(runtime.Result{ExitCode: runtime.ExitTrap, Error: err.Error()})
				break
			}
			serve(current, r.Then(ptr, mem))
		case "exit":
			p.finish(runtime.GuestExit(uint32(int32(data.Get("code").Int()))))
		case "trap":
			p.finish(runtime.Result{ExitCode: runtime.ExitTrap, Error: data.Get("message").String()})
		}
		return nil
	})
	onError := js.FuncOf(func(_ js.Value, a []js.Value) any {
		msg := "worker error"
		if m := a[0].Get("message"); m.Type() == js.TypeString {
			msg = m.String()
		}
		p.finish(runtime.Result{ExitCode: runtime.ExitTrap, Error: msg})
		return nil
	})
	p.funcs = []js.Func{onMessage, onError}
	worker.Set("onmessage", onMessage)
	worker.Set("onerror", onError)

	funcs := make([]any, len(imports))
	for i, f := range imports {
		funcs[i] = []any{f.Module, f.Name, f.Params, f.Result}
	}
	msg := js.Global().Get("Object").New()
	msg.Set("module", mod)
	msg.Set("ctl", ctl)
	msg.Set("funcs", js.ValueOf(funcs))
	worker.Call("postMessage", msg)
	return p
}
