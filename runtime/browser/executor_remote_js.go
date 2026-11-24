//go:build js

package browser

import (
	"syscall/js"

	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
)

// hostEvents is the executor's side of the control port: what the host worker
// reports about a guest it serves (Proc.Emit of a module, a trap).
func (e *Executor) hostEvents(d js.Value) {
	if d.Get("t").String() != "event" {
		return
	}
	e.hostMu.Lock()
	p := e.remote[d.Get("id").Int()]
	e.hostMu.Unlock()
	if p == nil {
		return
	}
	module, action := d.Get("module").String(), d.Get("action").String()
	attrs := map[string]string{}
	if a := d.Get("attrs"); a.Truthy() {
		keys := js.Global().Get("Object").Call("keys", a)
		for i := 0; i < keys.Length(); i++ {
			k := keys.Index(i).String()
			attrs[k] = a.Get(k).String()
		}
	}
	if module == "host-worker" {
		// The host worker could not serve the guest (a trap in a module, a bad
		// import, an engine and a host worker with different module lists): the
		// guest ends as it would on a trap in the engine's own worker.
		if action == "trap" || action == "bad-import" || action == "bad-realloc" || action == "mismatch" || action == "attach-refused" || action == "call-without-attach" {
			p.finish(runtime.Result{ExitCode: runtime.ExitTrap, Error: "gpu host worker: " + action + " " + attrs["error"] + attrs["fn"]})
		}
		return
	}
	p.proc.Emit(module, action, attrs)
}

// attachRemote gives a new guest a port to the GPU host worker, when the page
// made one (SetHostPort) and Options.RemoteModules names modules: the host
// worker gets the guest's end of a MessageChannel, its container label and
// capabilities; the guest's worker gets the other end and a second control
// block. It returns the "remote" field of the guest's start message and the
// port to transfer with it, or an undefined value when the guest's imports all
// stay with this worker.
func (e *Executor) attachRemote(p *process, proc *wasi.Proc, nImports int) (remote js.Value, transfer js.Value) {
	if len(e.opt.RemoteModules) == 0 {
		return js.Undefined(), js.Undefined()
	}
	e.hostMu.Lock()
	host := e.host
	if !host.Truthy() {
		e.hostMu.Unlock()
		return js.Undefined(), js.Undefined()
	}
	e.nextID++
	id := e.nextID
	if e.remote == nil {
		e.remote = map[int]*process{}
	}
	e.remote[id] = p
	e.hostMu.Unlock()

	g := js.Global()
	ch := g.Get("MessageChannel").New()
	mine, theirs := ch.Get("port1"), ch.Get("port2")
	ctl := g.Get("SharedArrayBuffer").New(ctlSize)
	mods := make([]any, len(e.opt.RemoteModules))
	for i, m := range e.opt.RemoteModules {
		mods[i] = m
	}
	caps := make([]any, 0, 2)
	for _, c := range proc.Caps() {
		caps = append(caps, c)
	}
	m := g.Get("Object").New()
	m.Set("t", "port")
	m.Set("id", id)
	m.Set("port", theirs)
	m.Set("label", proc.Label())
	m.Set("caps", js.ValueOf(caps))
	m.Set("n", nImports)
	host.Call("postMessage", m, js.ValueOf([]any{theirs}))
	p.detach = func() {
		e.hostMu.Lock()
		delete(e.remote, id)
		e.hostMu.Unlock()
		host.Call("postMessage", js.ValueOf(map[string]any{"t": "detach", "id": id}))
	}

	r := g.Get("Object").New()
	r.Set("ctl", ctl)
	r.Set("port", mine)
	r.Set("mods", js.ValueOf(mods))
	r.Set("label", proc.Label())
	return r, js.ValueOf([]any{mine})
}
