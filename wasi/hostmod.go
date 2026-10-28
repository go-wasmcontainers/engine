package wasi

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
)

// Host modules: import modules served beside wasi_snapshot_preview1, by
// the same Proc and the same executors (wazero natively, the SAB bridge in
// the browser). wasi-nn, wasi:webgpu (lowered by the canonical ABI) and the
// WASIX process calls are host modules.
//
// A module declares typed functions once; each process that calls into it
// gets its own Instance, opened at the first call and closed with the
// process, so per-process state (a GPU device, a child process table)
// lives and dies with the process.

// Value types beyond I32 and I64.
const (
	F32 = 'f'
	F64 = 'F'
)

// MaxParams bounds a host function's core parameters: the canonical ABI
// flattens at most 16 (MAX_FLAT_PARAMS), plus one return-area pointer.
const MaxParams = 17

// HostFunc is one function of a host module, with its core-wasm signature.
type HostFunc struct {
	Name string
	// Params has one of I32, I64, F32, F64 per parameter.
	Params string
	// Result is "" (no result) or one of I32, I64, F32, F64. The value is
	// Result.Ret; an F32 or F64 travels as its IEEE-754 bits.
	Result string
}

// HostModule is an import module a guest may link against.
type HostModule interface {
	// Module is the import module name, e.g. "wasi_ephemeral_nn".
	Module() string
	// Funcs lists the functions, in an order the module indexes by.
	Funcs() []HostFunc
	// Open makes the module's state for process p, at p's first call into
	// it. It never fails: a process denied the module (no capability) gets
	// an Instance that answers with the module's own error values.
	Open(p *Proc) Instance
}

// Instance is a host module's state in one process.
type Instance interface {
	// Call runs function fn of Funcs with one value per parameter (I32
	// in the low 32 bits, F32/F64 as bits).
	Call(fn int, args []uint64, m Memory) Result
	// Close releases the state when the process ends. It may run while a
	// Wait the instance returned is still pending; a later Resume must
	// then fail rather than touch released state.
	Close()
}

// Realloc asks the executor to allocate Size bytes aligned to Align in the
// guest, with the guest's cabi_realloc export (the canonical ABI's
// allocator), and to return Then(ptr) instead. The executor checks the
// block with CheckBlock first; a guest without the export, or a block
// outside its memory, traps the guest.
type Realloc struct {
	Align, Size uint32
	Then        func(ptr uint32, m Memory) Result
}

// CheckBlock refuses a block a guest's allocator returned unless it is
// aligned and lies inside a memory of memSize bytes. The guest chooses
// ptr, so the sum is computed without overflow.
func CheckBlock(ptr, align, size uint32, memSize uint64) error {
	if align > 1 && ptr%align != 0 {
		return fmt.Errorf("cabi_realloc returned %#x, not aligned to %d", ptr, align)
	}
	if uint64(ptr)+uint64(size) > memSize {
		return fmt.Errorf("cabi_realloc returned %#x+%d, outside memory", ptr, size)
	}
	return nil
}

// Import is one entry of a process's import table.
type Import struct {
	Module, Name   string
	Params, Result string
}

// PreviewModule is the import module of Funcs.
const PreviewModule = "wasi_snapshot_preview1"

// Imports returns the import table an executor defines and indexes by:
// Funcs first (wasi_snapshot_preview1, result I32 except proc_exit), then
// each module's functions in order. It refuses a malformed module: an
// empty or duplicate module name, PreviewModule itself, a duplicate
// function name, a bad type or more than MaxParams parameters.
func Imports(mods []HostModule) ([]Import, error) {
	out := make([]Import, 0, len(Funcs))
	for _, f := range Funcs {
		r := string(rune(I32))
		if f.NoResult {
			r = ""
		}
		out = append(out, Import{Module: PreviewModule, Name: f.Name, Params: f.Params, Result: r})
	}
	seen := map[string]bool{PreviewModule: true}
	for _, m := range mods {
		name := m.Module()
		if name == "" || seen[name] {
			return nil, fmt.Errorf("host module %q: empty or defined twice", name)
		}
		seen[name] = true
		fnames := map[string]bool{}
		for _, f := range m.Funcs() {
			if f.Name == "" || fnames[f.Name] {
				return nil, fmt.Errorf("host module %q: function %q empty or defined twice", name, f.Name)
			}
			fnames[f.Name] = true
			if len(f.Params) > MaxParams || strings.Trim(f.Params, "iIfF") != "" {
				return nil, fmt.Errorf("host module %q: function %q: bad params %q", name, f.Name, f.Params)
			}
			if len(f.Result) > 1 || strings.Trim(f.Result, "iIfF") != "" {
				return nil, fmt.Errorf("host module %q: function %q: bad result %q", name, f.Name, f.Result)
			}
			out = append(out, Import{Module: name, Name: f.Name, Params: f.Params, Result: f.Result})
		}
	}
	return out, nil
}

// hostState is a process's host modules and their opened instances.
type hostState struct {
	mods  []HostModule
	base  []int // index in the import table of each module's first function
	mu    sync.Mutex
	inst  []Instance
	close bool
}

func newHostState(mods []HostModule) *hostState {
	h := &hostState{mods: mods, base: make([]int, len(mods)), inst: make([]Instance, len(mods))}
	n := len(Funcs)
	for i, m := range mods {
		h.base[i] = n
		n += len(m.Funcs())
	}
	return h
}

// errClosed is what a call into a closed process's module returns.
var errClosed = errors.New("process closed")

// CallImport runs entry i of the table Imports(cfg.Modules) returned.
// Entries past Funcs go to their module's Instance, opened on first use.
func (p *Proc) CallImport(i int, args []uint64, m Memory) Result {
	if i < len(Funcs) {
		return p.Call(i, args, m)
	}
	h := p.host
	for k := len(h.mods) - 1; k >= 0; k-- {
		if i < h.base[k] {
			continue
		}
		fs := h.mods[k].Funcs()
		fn := i - h.base[k]
		if fn >= len(fs) || len(args) != len(fs[fn].Params) {
			break
		}
		inst, err := h.open(k, p)
		if err != nil {
			return Result{Trap: err.Error()}
		}
		return inst.Call(fn, args, m)
	}
	return Result{Trap: fmt.Sprintf("no host function at import %d", i)}
}

// open returns module k's instance, opening it on first use. Open runs
// without the lock (it may look up other instances of this process, or of
// its parent); if two calls race, the first instance stored wins and the
// other is closed.
func (h *hostState) open(k int, p *Proc) (Instance, error) {
	h.mu.Lock()
	if h.close {
		h.mu.Unlock()
		return nil, errClosed
	}
	if in := h.inst[k]; in != nil {
		h.mu.Unlock()
		return in, nil
	}
	h.mu.Unlock()
	in := h.mods[k].Open(p)
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case h.close:
		in.Close()
		return nil, errClosed
	case h.inst[k] != nil:
		in.Close()
		return h.inst[k], nil
	}
	h.inst[k] = in
	return in, nil
}

// closeAll closes the opened instances, last opened module first.
func (h *hostState) closeAll() {
	h.mu.Lock()
	h.close = true
	inst := slices.Clone(h.inst)
	h.mu.Unlock()
	for k := len(inst) - 1; k >= 0; k-- {
		if inst[k] != nil {
			inst[k].Close()
		}
	}
}

// HasCap reports whether the process was started with capability c (e.g.
// "gpu", from run --gpu).
func (p *Proc) HasCap(c string) bool { return slices.Contains(p.cfg.Caps, c) }

// Parent is the process that started this one (nil for a container's
// first process), so a module can share state across a process tree.
func (p *Proc) Parent() *Proc { return p.cfg.Parent }

// Instance returns the opened instance of the module named module in this
// process, or nil; a child's module may look up its parent's this way.
func (p *Proc) Instance(module string) Instance {
	h := p.host
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, m := range h.mods {
		if m.Module() == module {
			return h.inst[k]
		}
	}
	return nil
}
