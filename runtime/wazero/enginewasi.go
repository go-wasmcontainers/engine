package wazero

import (
	"context"
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

// instantiateEngineWASI defines wasi_snapshot_preview1 from package wasi.
func instantiateEngineWASI(ctx context.Context, r wazero.Runtime) (api.Closer, error) {
	b := r.NewHostModuleBuilder("wasi_snapshot_preview1")
	for i, f := range wasi.Funcs {
		i, f := i, f
		params := make([]api.ValueType, len(f.Params))
		for j := range f.Params {
			params[j] = api.ValueTypeI32
			if f.Params[j] == wasi.I64 {
				params[j] = api.ValueTypeI64
			}
		}
		var results []api.ValueType
		if !f.NoResult {
			results = []api.ValueType{api.ValueTypeI32}
		}
		b.NewFunctionBuilder().WithGoModuleFunction(api.GoModuleFunc(func(ctx context.Context, mod api.Module, stack []uint64) {
			p, _ := ctx.Value(procKey{}).(*wasi.Proc)
			if p == nil {
				if !f.NoResult {
					stack[0] = wasi.ENOSYS
				}
				return
			}
			in := append([]uint64(nil), stack[:len(f.Params)]...)
			res := p.Call(i, in, wazeroMemory{mod.Memory()})
			if res.Exit {
				_ = mod.CloseWithExitCode(ctx, res.Code)
				panic(sys.NewExitError(res.Code))
			}
			if res.Sleep > 0 {
				t := time.NewTimer(res.Sleep)
				select {
				case <-t.C:
				case <-ctx.Done():
					t.Stop()
				}
			}
			if !f.NoResult {
				stack[0] = uint64(res.Errno)
			}
		}), params, results).Export(f.Name)
	}
	return b.Instantiate(ctx)
}
