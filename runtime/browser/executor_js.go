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
	e.mu.Lock()
	e.tick++
	if c, ok := e.compiled[key]; ok {
		c.used = e.tick
		e.mu.Unlock()
		return c.mod, nil
	}
	e.mu.Unlock()
	// The cap must be in the binary: a browser cannot bound a memory the
	// module defines. Shared, so this worker can serve WASI on it.
	patched, err := wasmbin.LimitMemory(wasm, pages, true)
	if err != nil {
		return js.Value{}, fmt.Errorf("%w: %v", runtime.ErrInvalid, err)
	}
	mod, err := await(js.Global().Get("WebAssembly").Call("compile", uint8Array(patched)))
	if err != nil {
		return js.Value{}, fmt.Errorf("%w: %v", runtime.ErrInvalid, err)
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
}

func (p *process) Wait() runtime.Result { <-p.done; return p.res }

func (p *process) finish(r runtime.Result) {
	p.once.Do(func() {
		if p.timer != nil {
			p.timer.Stop()
		}
		p.worker.Call("terminate")
		p.res = r
		close(p.done)
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
	proc := wasi.New(wasi.Config{Args: args, Env: spec.Env, FS: spec.FS, Stdin: spec.Stdin, Stdout: spec.Stdout, Stderr: spec.Stderr})

	ctl := js.Global().Get("SharedArrayBuffer").New(256)
	i32 := js.Global().Get("Int32Array").New(ctl)
	ctlU8 := js.Global().Get("Uint8Array").New(ctl, 0, 192)
	atomics := js.Global().Get("Atomics")
	worker := js.Global().Get("Worker").New(e.opt.WorkerURL)
	p := &process{worker: worker, done: make(chan struct{})}
	mem := jsMemory{mem: js.Undefined()}
	ctlBuf := make([]byte, 192)

	onMessage := js.FuncOf(func(_ js.Value, a []js.Value) any {
		data := a[0].Get("data")
		switch data.Get("t").String() {
		case "memory":
			mem = jsMemory{mem: data.Get("memory")}
		case "call":
			js.CopyBytesToGo(ctlBuf, ctlU8)
			fn := int(int32(binary.LittleEndian.Uint32(ctlBuf[4:])))
			var res wasi.Result
			if fn < 0 || fn >= len(wasi.Funcs) {
				res = wasi.Result{Errno: wasi.ENOSYS}
			} else {
				n := len(wasi.Funcs[fn].Params)
				callArgs := make([]uint64, n)
				for i := 0; i < n; i++ {
					callArgs[i] = binary.LittleEndian.Uint64(ctlBuf[64+8*i:])
				}
				res = proc.Call(fn, callArgs, mem)
			}
			flags := 0
			if res.Exit {
				flags = 1
			}
			sleep := res.Sleep.Microseconds()
			if sleep > math.MaxInt32 {
				sleep = math.MaxInt32
			}
			i32.SetIndex(2, int(res.Errno))
			i32.SetIndex(3, flags)
			i32.SetIndex(4, int(sleep))
			i32.SetIndex(5, int(int32(res.Code)))
			atomics.Call("store", i32, 0, 2)
			atomics.Call("notify", i32, 0)
		case "exit":
			p.finish(runtime.Result{ExitCode: data.Get("code").Int()})
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

	funcs := make([]any, len(wasi.Funcs))
	for i, f := range wasi.Funcs {
		funcs[i] = []any{f.Name, f.NoResult}
	}
	msg := js.Global().Get("Object").New()
	msg.Set("module", mod)
	msg.Set("ctl", ctl)
	msg.Set("funcs", js.ValueOf(funcs))
	worker.Call("postMessage", msg)

	if spec.Timeout > 0 {
		p.timer = time.AfterFunc(spec.Timeout, func() {
			p.finish(runtime.Result{ExitCode: runtime.ExitKilled, TimedOut: true, Error: "wall-clock timeout"})
		})
	}
	context.AfterFunc(ctx, p.Kill)
	return p, nil
}
