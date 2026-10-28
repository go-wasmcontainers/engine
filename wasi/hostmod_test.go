package wasi_test

import (
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/wasi"
	"github.com/go-wasmcontainers/engine/wasi/hostmodtest"
)

type fakeMod struct {
	name  string
	funcs []wasi.HostFunc
}

func (f fakeMod) Module() string                      { return f.name }
func (f fakeMod) Funcs() []wasi.HostFunc              { return f.funcs }
func (f fakeMod) Open(*wasi.Proc) wasi.Instance       { return nil }
func one(name, params, result string) []wasi.HostFunc { return []wasi.HostFunc{{name, params, result}} }

func TestImportsRefusesMalformedModules(t *testing.T) {
	for _, c := range []struct {
		mods []wasi.HostModule
		want string
	}{
		{[]wasi.HostModule{fakeMod{"", one("f", "", "")}}, "empty or defined twice"},
		{[]wasi.HostModule{fakeMod{wasi.PreviewModule, one("f", "", "")}}, "empty or defined twice"},
		{[]wasi.HostModule{fakeMod{"a", one("f", "", "")}, fakeMod{"a", one("g", "", "")}}, "empty or defined twice"},
		{[]wasi.HostModule{fakeMod{"a", append(one("f", "", ""), one("f", "i", "")...)}}, `function "f"`},
		{[]wasi.HostModule{fakeMod{"a", one("", "", "")}}, `function ""`},
		{[]wasi.HostModule{fakeMod{"a", one("f", "x", "")}}, "bad params"},
		{[]wasi.HostModule{fakeMod{"a", one("f", strings.Repeat("i", wasi.MaxParams+1), "")}}, "bad params"},
		{[]wasi.HostModule{fakeMod{"a", one("f", "", "ii")}}, "bad result"},
		{[]wasi.HostModule{fakeMod{"a", one("f", "", "v")}}, "bad result"},
	} {
		if _, err := wasi.Imports(c.mods); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Imports(%v) = %v, want an error with %q", c.mods, err, c.want)
		}
	}
	imps, err := wasi.Imports([]wasi.HostModule{fakeMod{"a", one("f", strings.Repeat("F", wasi.MaxParams), "f")}})
	if err != nil {
		t.Fatal(err)
	}
	last := imps[len(imps)-1]
	if len(imps) != len(wasi.Funcs)+1 || last.Module != "a" || last.Name != "f" || imps[0].Module != wasi.PreviewModule || imps[0].Result != "i" {
		t.Errorf("table: %d entries, last %+v, first %+v", len(imps), last, imps[0])
	}
}

type mem []byte

func (m mem) Read(off, n uint32) ([]byte, bool) {
	if uint64(off)+uint64(n) > uint64(len(m)) {
		return nil, false
	}
	return m[off : off+n], true
}

func (m mem) Write(off uint32, b []byte) bool {
	if uint64(off)+uint64(len(b)) > uint64(len(m)) {
		return false
	}
	copy(m[off:], b)
	return true
}

func TestCallImport(t *testing.T) {
	mod := &hostmodtest.Module{}
	parent := wasi.New(wasi.Config{FS: rootfs.New()})
	p := wasi.New(wasi.Config{FS: rootfs.New(), Modules: []wasi.HostModule{mod}, Caps: []string{"gpu"}, Parent: parent})
	base := len(wasi.Funcs)
	m := make(mem, 64)

	if p.Instance("wctr_test") != nil {
		t.Error("an instance exists before the first call")
	}
	if r := p.CallImport(base, []uint64{40, 2}, m); r.Ret != 42 {
		t.Errorf("add: %+v", r)
	}
	if r := p.CallImport(base+7, nil, m); r.Ret != 1 {
		t.Errorf("cap with gpu: %+v", r)
	}
	if p.Instance("wctr_test") == nil || p.Instance("nope") != nil || mod.Opened.Load() != 1 {
		t.Errorf("instances: opened %d", mod.Opened.Load())
	}
	if !p.HasCap("gpu") || p.HasCap("tpu") || p.Parent() != parent || parent.Parent() != nil {
		t.Error("caps or parent")
	}
	// The preview functions still answer through the same entry point.
	if r := p.CallImport(wasi.FuncIndex["sched_yield"], nil, m); r.Errno != 0 || r.Trap != "" {
		t.Errorf("sched_yield: %+v", r)
	}
	// Wrong arity and unknown indices trap rather than run.
	for _, c := range []struct {
		i    int
		args []uint64
	}{{base, []uint64{1}}, {base + 8, nil}, {-1 + base + 100, nil}} {
		if r := p.CallImport(c.i, c.args, m); r.Trap == "" {
			t.Errorf("CallImport(%d, %v) = %+v, want a trap", c.i, c.args, r)
		}
	}
	p.Close()
	if mod.Closed.Load() != 1 {
		t.Errorf("closed %d instances, want 1", mod.Closed.Load())
	}
	if r := p.CallImport(base, []uint64{1, 2}, m); r.Trap == "" {
		t.Errorf("call after Close: %+v, want a trap", r)
	}
	parent.Close()
}

func TestCheckBlock(t *testing.T) {
	for _, c := range []struct {
		p, align, size uint32
		mem            uint64
		ok             bool
	}{
		{0, 1, 0, 0, true},
		{0, 1, 1, 0, false},
		{4, 8, 1, 64, false},
		{8, 8, 56, 64, true},
		{8, 8, 57, 64, false},
		{0xffffffff, 1, 1, 0xffffffff, false},
		{0xffffffff, 1, 1, 1 << 32, true},
	} {
		if got := wasi.CheckBlock(c.p, c.align, c.size, c.mem) == nil; got != c.ok {
			t.Errorf("CheckBlock(%#x, %d, %d, %d) = %v, want %v", c.p, c.align, c.size, c.mem, got, c.ok)
		}
	}
}

// FuzzCheckBlock: a block is accepted exactly when it is aligned and its
// end, computed in 64 bits, is inside memory.
func FuzzCheckBlock(f *testing.F) {
	f.Add(uint32(0xfffffff0), uint32(16), uint32(0x20), uint64(0xffffffff))
	f.Fuzz(func(t *testing.T, p, align, size uint32, memSize uint64) {
		want := uint64(p)+uint64(size) <= memSize && (align <= 1 || p%align == 0)
		if got := wasi.CheckBlock(p, align, size, memSize) == nil; got != want {
			t.Fatalf("CheckBlock(%#x, %d, %d, %d) = %v, want %v", p, align, size, memSize, got, want)
		}
	})
}
