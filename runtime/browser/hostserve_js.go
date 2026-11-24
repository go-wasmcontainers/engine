//go:build js

package browser

import (
	"encoding/binary"
	"fmt"
	"math"
	"sync"
	"syscall/js"
	"time"

	"github.com/go-wasmcontainers/engine/wasi"
)

// HostWorker serves the host calls of the modules the page's GPU host worker owns
// (Options.RemoteModules): wasi:webgpu, wasi-gfx:surface, wasi_webgpu_batch,
// wasi-nn. It runs in its own Web Worker, the same engine.wasm started with a
// different role (go-wasmcontainers/engine#84), so that a guest's GPU call is
// served by a thread that runs nothing else: the engine's worker also runs Go's
// garbage collector and every other call of every container, and a single-threaded
// game that waits for it in a present or a submit stops feeding its audio.
//
// The wiring, all by messages the engine's executor sends (see attachRemote):
//
//	page   -> host worker   {t:"engine", port}       the control port to the engine
//	engine -> host worker   {t:"port", id, port, label, caps, mods, n}  (on that port)
//	                        one guest's port, its container label and capabilities
//	guest  -> host worker   {t:"attach", memory, ctl, label, n}   (on the guest's port)
//	guest  -> host worker   {t:"call"} | {t:"realloc", error} | {t:"detach"}
//	engine -> host worker   {t:"detach", id}         a process the engine ended
//	host   -> engine        {t:"event", id, module, action, attrs}  (Proc.Emit)
//
// The worker keeps one wasi.Proc per guest process: a module Instance is keyed by
// the *wasi.Proc (webgpu.Module.Open), so each container (and each of its WASIX
// children) has its own device and its own state, as in the engine's worker. The
// Proc holds only the label and the capabilities: no file system, no descriptors.
type HostWorker struct {
	mods    []wasi.HostModule
	imports []wasi.Import

	mu     sync.Mutex
	guests map[int]*hostGuest
	engine js.Value // the control port to the engine's executor
	funcs  []js.Func
}

type hostGuest struct {
	id   int
	proc *wasi.Proc
	port js.Value
	ctl  js.Value
	once sync.Once
	done chan struct{} // closed when the guest is detached

	funcs []js.Func
}

// NewHostWorker returns a host worker for the same host modules, in the same
// order, as the engine's executor (the import indexes are shared).
func NewHostWorker(mods []wasi.HostModule) (*HostWorker, error) {
	imports, err := wasi.Imports(mods)
	if err != nil {
		return nil, err
	}
	return &HostWorker{mods: mods, imports: imports, guests: map[int]*hostGuest{}}, nil
}

// Handle takes a message the page posted to this worker and reports whether it
// was the host worker's: {t:"engine", port}.
func (h *HostWorker) Handle(data js.Value) bool {
	if data.Get("t").String() != "engine" {
		return false
	}
	port := data.Get("port")
	if !port.Truthy() {
		return true
	}
	h.mu.Lock()
	h.engine = port
	h.mu.Unlock()
	f := js.FuncOf(func(_ js.Value, a []js.Value) any {
		h.onEngine(a[0].Get("data"))
		return nil
	})
	h.funcs = append(h.funcs, f)
	port.Set("onmessage", f)
	return true
}

func (h *HostWorker) onEngine(d js.Value) {
	switch d.Get("t").String() {
	case "port":
		h.attachPort(d)
	case "detach":
		h.mu.Lock()
		g := h.guests[d.Get("id").Int()]
		h.mu.Unlock()
		if g != nil {
			h.detach(g)
		}
	}
}

func (h *HostWorker) attachPort(d js.Value) {
	n := d.Get("n").Int()
	if n != len(h.imports) {
		// An engine and a host worker built from different module lists would index
		// different functions: refuse every call of this guest.
		d.Get("port").Call("close")
		h.event(d.Get("id").Int(), "host-worker", "mismatch", map[string]string{"engine": fmt.Sprint(n), "host": fmt.Sprint(len(h.imports))})
		return
	}
	var caps []string
	if c := d.Get("caps"); c.Truthy() {
		for i := 0; i < c.Length(); i++ {
			caps = append(caps, c.Index(i).String())
		}
	}
	g := &hostGuest{id: d.Get("id").Int(), port: d.Get("port"), done: make(chan struct{})}
	g.proc = wasi.New(wasi.Config{Modules: h.mods, Caps: caps, Label: d.Get("label").String(),
		OnEvent: func(module, action string, attrs map[string]string) { h.event(g.id, module, action, attrs) }})
	h.mu.Lock()
	h.guests[g.id] = g
	h.mu.Unlock()
	h.serve(g)
}

// event reports to the engine what a module said about a guest's container.
func (h *HostWorker) event(id int, module, action string, attrs map[string]string) {
	h.mu.Lock()
	e := h.engine
	h.mu.Unlock()
	if !e.Truthy() {
		return
	}
	a := map[string]any{}
	for k, v := range attrs {
		a[k] = v
	}
	e.Call("postMessage", js.ValueOf(map[string]any{"t": "event", "id": id, "module": module, "action": action, "attrs": a}))
}

func (h *HostWorker) detach(g *hostGuest) {
	g.once.Do(func() {
		close(g.done)
		h.mu.Lock()
		delete(h.guests, g.id)
		h.mu.Unlock()
		g.port.Call("close")
		go func() {
			g.proc.Close() // the module instances: the device, the canvases
			for _, f := range g.funcs {
				f.Release()
			}
		}()
	})
}

// serve is the engine executor's serve loop (run in executor_js.go) for one
// guest: the guest's calls arrive on its port, the arguments are in its control
// block, the answer goes back in the same block and wakes the guest's
// Atomics.wait. The same continuation rules apply: a result that waits (a
// promise) is finished on a goroutine, a result that needs memory in the guest
// asks its cabi_realloc (the guest answers with a "realloc" message).
func (h *HostWorker) serve(g *hostGuest) {
	atomics := js.Global().Get("Atomics")
	var (
		mem     = jsMemory{mem: js.Undefined()}
		ctl     js.Value
		i32     js.Value
		ctlU8   js.Value
		pending *wasi.Realloc
		current = -1
		ctlBuf  = make([]byte, ctlSize)
	)
	f64 := js.Value{}
	stamp := func(off int) {
		if f64.Truthy() {
			f64.SetIndex(off/8, js.Global().Get("performance").Get("timeOrigin").Float()+js.Global().Get("performance").Call("now").Float())
		}
	}
	var serve func(fn int, res wasi.Result)
	reply := func(fn int, res wasi.Result) {
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
		stamp(ctlAnswer)
		atomics.Call("store", i32, ctlState, stateDone)
		atomics.Call("notify", i32, ctlState)
	}
	serve = func(fn int, res wasi.Result) {
		switch {
		case res.Trap != "":
			// A trap ends the guest: the engine's executor kills it when it hears
			// of it; here the guest is answered with an error it cannot ignore.
			h.event(g.id, "host-worker", "trap", map[string]string{"error": res.Trap})
			i32.SetIndex(ctlErrno, 1)
			stamp(ctlAnswer)
			atomics.Call("store", i32, ctlState, stateDone)
			atomics.Call("notify", i32, ctlState)
		case res.Realloc != nil:
			pending = res.Realloc
			i32.SetIndex(ctlFlags, flagRealloc)
			i32.SetIndex(ctlAlign, int(int32(res.Realloc.Align)))
			i32.SetIndex(ctlSizeArg, int(int32(res.Realloc.Size)))
			stamp(ctlAnswer)
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
					case <-g.done:
						return
					}
					res = res.Resume(mem)
				}
				serve(fn, res)
			}(res, mem)
		}
	}
	onMessage := js.FuncOf(func(_ js.Value, a []js.Value) any {
		d := a[0].Get("data")
		switch d.Get("t").String() {
		case "attach":
			mem = jsMemory{mem: d.Get("memory")}
			ctl = d.Get("ctl")
			i32 = js.Global().Get("Int32Array").New(ctl)
			ctlU8 = js.Global().Get("Uint8Array").New(ctl, 0, ctlSize)
			f64 = js.Global().Get("Float64Array").New(ctl)
		case "call":
			if !ctl.Truthy() {
				break // a call before its attach cannot happen: one port keeps order
			}
			stamp(ctlServed)
			js.CopyBytesToGo(ctlBuf, ctlU8)
			fn := int(int32(binary.LittleEndian.Uint32(ctlBuf[4*ctlFunc:])))
			if fn < len(wasi.Funcs) || fn >= len(h.imports) {
				h.event(g.id, "host-worker", "bad-import", map[string]string{"fn": fmt.Sprint(fn)})
				i32.SetIndex(ctlErrno, 1)
				atomics.Call("store", i32, ctlState, stateDone)
				atomics.Call("notify", i32, ctlState)
				break
			}
			current = fn
			n := len(h.imports[fn].Params)
			callArgs := make([]uint64, n)
			for i := 0; i < n; i++ {
				callArgs[i] = binary.LittleEndian.Uint64(ctlBuf[ctlArgs+8*i:])
			}
			serve(fn, g.proc.CallImport(fn, callArgs, mem))
		case "realloc":
			r := pending
			pending = nil
			if r == nil || current < 0 || d.Get("error").Truthy() {
				i32.SetIndex(ctlErrno, 1)
				atomics.Call("store", i32, ctlState, stateDone)
				atomics.Call("notify", i32, ctlState)
				break
			}
			js.CopyBytesToGo(ctlBuf, ctlU8)
			ptr := binary.LittleEndian.Uint32(ctlBuf[4*ctlPtr:])
			if err := wasi.CheckBlock(ptr, r.Align, r.Size, mem.size()); err != nil {
				h.event(g.id, "host-worker", "bad-realloc", map[string]string{"error": err.Error()})
				i32.SetIndex(ctlErrno, 1)
				atomics.Call("store", i32, ctlState, stateDone)
				atomics.Call("notify", i32, ctlState)
				break
			}
			serve(current, r.Then(ptr, mem))
		case "detach":
			h.detach(g)
		}
		return nil
	})
	g.funcs = append(g.funcs, onMessage)
	g.port.Set("onmessage", onMessage)
}
