package wazero

import (
	"context"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
	"github.com/go-wasmcontainers/engine/wasi/hostmodtest"
)

// TestHostModule runs testdata/wasm/hostmod against the test host module:
// typed values both ways, a host-allocated result (cabi_realloc called
// from inside the import), a waiting call, a trap, a capability, and one
// instance per process, closed when it ends.
func TestHostModule(t *testing.T) {
	img := lower(t, "hostmod")
	mod := &hostmodtest.Module{}
	o := opts()
	o.HostModules = []wasi.HostModule{mod}
	e := New(o)
	defer e.Close(context.Background())

	for _, c := range []struct {
		args []string
		caps []string
		out  string
		code int
		err  string
	}{
		{args: []string{"types"}, out: "42 -2.25 -1.5 -2199023255552\n"},
		{args: []string{"realloc"}, out: "hello from the host\n"},
		{args: []string{"wait"}, out: "7\n"},
		{args: []string{"cap"}, out: "0\n"},
		{args: []string{"cap"}, caps: []string{"gpu"}, out: "1\n"},
		{args: []string{"trap"}, code: runtime.ExitTrap, err: "boom: the test module traps here"},
	} {
		id := "c-" + strings.Join(c.args, "-")
		r := start(t, e, runtime.Spec{ID: id, FS: img.Clone(rootfs.Quota{}), Path: "/bin/hostmod.wasm",
			Args: append([]string{"hostmod"}, c.args...), Caps: c.caps})
		if r.stdout != c.out || r.res.ExitCode != c.code || !strings.Contains(r.res.Error, c.err) {
			t.Errorf("%v: stdout %q, exit %d, error %q; want %q, %d, %q (stderr %q)",
				c.args, r.stdout, r.res.ExitCode, r.res.Error, c.out, c.code, c.err, r.stderr)
		}
		// The module sees the container it serves (wasi.Proc.Label).
		if l, _ := mod.Label.Load().(string); l != id {
			t.Errorf("%v: label %q, want %q", c.args, l, id)
		}
	}
	if o, c := mod.Opened.Load(), mod.Closed.Load(); o != 6 || c != 6 {
		t.Errorf("opened %d, closed %d instances; want 6 and 6", o, c)
	}
}

// TestHostModuleNotImported: a process that never calls the module never
// opens it.
func TestHostModuleNotImported(t *testing.T) {
	img := lower(t, "hello")
	mod := &hostmodtest.Module{}
	o := opts()
	o.HostModules = []wasi.HostModule{mod}
	e := New(o)
	defer e.Close(context.Background())
	if r := start(t, e, runtime.Spec{FS: img.Clone(rootfs.Quota{}), Path: "/bin/hello.wasm"}); r.res.ExitCode != 0 {
		t.Fatalf("hello: %+v", r)
	}
	if n := mod.Opened.Load(); n != 0 {
		t.Errorf("opened %d instances for a process that does not import the module", n)
	}
}
