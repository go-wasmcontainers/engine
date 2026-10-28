package wazero

import (
	"context"
	"errors"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"

	"github.com/go-wasmcontainers/engine/wasi"
)

type procKey struct{}

func withProc(ctx context.Context, p *wasi.Proc) context.Context {
	return context.WithValue(ctx, procKey{}, p)
}

// wazeroMemory adapts a module's memory to wasi.Memory.
type wazeroMemory struct{ m api.Memory }

func (w wazeroMemory) Read(off, n uint32) ([]byte, bool) { return w.m.Read(off, n) }
func (w wazeroMemory) Write(off uint32, b []byte) bool   { return w.m.Write(off, b) }

// wait blocks until ch closes or d (if not zero) passes; false on a kill.
func wait(ctx context.Context, ch <-chan struct{}, d time.Duration) bool {
	var timeout <-chan time.Time
	if d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		timeout = t.C
	}
	select {
	case <-ch:
	case <-timeout:
	case <-ctx.Done():
		return false
	}
	return true
}

func valueType(c byte) api.ValueType {
	switch c {
	case wasi.I64:
		return api.ValueTypeI64
	case wasi.F32:
		return api.ValueTypeF32
	case wasi.F64:
		return api.ValueTypeF64
	}
	return api.ValueTypeI32
}

// errNoRealloc traps a guest a host module asked to allocate in, when the
// guest exports no cabi_realloc.
var errNoRealloc = errors.New("host module needs the guest's cabi_realloc export, which it lacks")

// realloc calls the guest's cabi_realloc(0, 0, align, size) from inside a
// host call (measured safe for Go wasip1 guests: go-wasmcontainers/gpu
// internal/reentry) and checks the block it returns.
func realloc(ctx context.Context, mod api.Module, r *wasi.Realloc) (uint32, error) {
	f := mod.ExportedFunction("cabi_realloc")
	if f == nil {
		return 0, errNoRealloc
	}
	res, err := f.Call(ctx, 0, 0, uint64(r.Align), uint64(r.Size))
	if err != nil {
		return 0, err
	}
	if len(res) != 1 {
		return 0, errors.New("cabi_realloc returned no pointer")
	}
	ptr := uint32(res[0])
	if err := wasi.CheckBlock(ptr, r.Align, r.Size, uint64(mod.Memory().Size())); err != nil {
		return 0, err
	}
	return ptr, nil
}

// instantiateEngineWASI defines wasi_snapshot_preview1 from package wasi,
// and each host module beside it.
func instantiateEngineWASI(ctx context.Context, r wazero.Runtime, mods []wasi.HostModule) ([]api.Closer, error) {
	imports, err := wasi.Imports(mods)
	if err != nil {
		return nil, err
	}
	builders := map[string]wazero.HostModuleBuilder{}
	var order []string
	for i, imp := range imports {
		b, ok := builders[imp.Module]
		if !ok {
			b = r.NewHostModuleBuilder(imp.Module)
			builders[imp.Module] = b
			order = append(order, imp.Module)
		}
		params := make([]api.ValueType, len(imp.Params))
		for j := range imp.Params {
			params[j] = valueType(imp.Params[j])
		}
		var results []api.ValueType
		if imp.Result != "" {
			results = []api.ValueType{valueType(imp.Result[0])}
		}
		preview := i < len(wasi.Funcs)
		b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			p, _ := ctx.Value(procKey{}).(*wasi.Proc)
			if p == nil {
				if !preview {
					panic(errors.New("host module " + imp.Module + " needs the engine's WASI host"))
				}
				if len(results) > 0 {
					stack[0] = wasi.ENOSYS
				}
				return
			}
			in := append([]uint64(nil), stack[:len(params)]...)
			mem := wazeroMemory{mod.Memory()}
			res := p.CallImport(i, in, mem)
			for {
				if res.Trap != "" {
					panic(errors.New(res.Trap))
				}
				if res.Exit {
					_ = mod.CloseWithExitCode(ctx, res.Code)
					panic(sys.NewExitError(res.Code))
				}
				if res.Realloc != nil {
					ptr, err := realloc(ctx, mod, res.Realloc)
					if err != nil {
						panic(err)
					}
					res = res.Realloc.Then(ptr, mem)
					continue
				}
				if res.Wait == nil {
					break
				}
				if !wait(ctx, res.Wait, res.Sleep) {
					res = wasi.Result{Errno: wasi.EINTR}
					break
				}
				res = res.Resume(mem)
			}
			if res.Sleep > 0 {
				t := time.NewTimer(res.Sleep)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
				}
			}
			if len(results) > 0 {
				if preview {
					stack[0] = uint64(res.Errno)
				} else {
					stack[0] = res.Ret
				}
			}
		}), params, results).Export(imp.Name)
	}
	var closers []api.Closer
	for _, name := range order {
		c, err := builders[name].Instantiate(ctx)
		if err != nil {
			for _, c := range closers {
				c.Close(ctx)
			}
			return nil, err
		}
		closers = append(closers, c)
	}
	return closers, nil
}
