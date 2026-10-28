// Package hostmodtest is a host module for tests ("wctr_test"): the
// executors' tests run testdata/wasm/hostmod against it, natively and in
// the browser, so both sides of the seam are exercised by one guest.
package hostmodtest

import (
	"encoding/binary"
	"math"
	"sync/atomic"
	"time"

	"github.com/go-wasmcontainers/engine/wasi"
)

// Module is the test module. Opened and Closed count instances.
type Module struct {
	Opened, Closed atomic.Int32
}

// Module implements wasi.HostModule.
func (*Module) Module() string { return "wctr_test" }

// Funcs implements wasi.HostModule.
func (*Module) Funcs() []wasi.HostFunc {
	return []wasi.HostFunc{
		{Name: "add", Params: "ii", Result: "i"},
		{Name: "mix", Params: "IfF", Result: "F"},
		{Name: "neg", Params: "f", Result: "f"},
		{Name: "wide", Params: "I", Result: "I"},
		{Name: "hello", Params: "i"},
		{Name: "later", Result: "i"},
		{Name: "boom"},
		{Name: "cap", Result: "i"},
	}
}

// Open implements wasi.HostModule.
func (m *Module) Open(p *wasi.Proc) wasi.Instance {
	m.Opened.Add(1)
	return &instance{m: m, gpu: p.HasCap("gpu")}
}

type instance struct {
	m   *Module
	gpu bool
}

func (i *instance) Close() { i.m.Closed.Add(1) }

func (i *instance) Call(fn int, a []uint64, m wasi.Memory) wasi.Result {
	switch fn {
	case 0:
		return wasi.Result{Ret: uint64(uint32(a[0]) + uint32(a[1]))}
	case 1:
		v := float64(int64(a[0])) + float64(math.Float32frombits(uint32(a[1]))) + math.Float64frombits(a[2])
		return wasi.Result{Ret: math.Float64bits(v)}
	case 2:
		return wasi.Result{Ret: uint64(math.Float32bits(-math.Float32frombits(uint32(a[0]))))}
	case 3:
		return wasi.Result{Ret: uint64(int64(a[0]) * 2)}
	case 4:
		msg := []byte("hello from the host")
		ret := uint32(a[0])
		return wasi.Result{Realloc: &wasi.Realloc{Align: 1, Size: uint32(len(msg)), Then: func(ptr uint32, m wasi.Memory) wasi.Result {
			var rec [8]byte
			binary.LittleEndian.PutUint32(rec[0:], ptr)
			binary.LittleEndian.PutUint32(rec[4:], uint32(len(msg)))
			if !m.Write(ptr, msg) || !m.Write(ret, rec[:]) {
				return wasi.Result{Trap: "hello: out of bounds"}
			}
			return wasi.Result{}
		}}}
	case 5:
		ch := make(chan struct{})
		time.AfterFunc(20*time.Millisecond, func() { close(ch) })
		return wasi.Result{Wait: ch, Resume: func(wasi.Memory) wasi.Result { return wasi.Result{Ret: 7} }}
	case 6:
		return wasi.Result{Trap: "boom: the test module traps here"}
	case 7:
		if i.gpu {
			return wasi.Result{Ret: 1}
		}
		return wasi.Result{}
	}
	return wasi.Result{Trap: "unknown function"}
}
