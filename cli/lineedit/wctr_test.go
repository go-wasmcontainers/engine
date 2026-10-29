package lineedit

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/api/inproc"
	"github.com/go-wasmcontainers/engine/api/server"
	"github.com/go-wasmcontainers/engine/cli"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/daemon"
	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
)

// TestWctrCompleter drives the editor against a real in-process engine:
// typing and Tab, as the playground and the dashboard do.
func TestWctrCompleter(t *testing.T) {
	ex := wazero.New(wazero.Options{CompilationCache: testutil.CompilationCache()})
	defer ex.Close(context.Background())
	d, err := daemon.New(daemon.Options{Backend: image.NewMemoryBackend(0), Containers: container.Options{Executor: ex}})
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &inproc.Transport{Handler: server.New(d, server.Info{Version: "t"})}}
	o := cli.Options{DefaultHost: "inproc://", Client: func(string) (*api.Client, error) { return api.NewClientWith(hc, "http://e"), nil }, NoCompletionCommand: true}
	layout := filepath.Join(t.TempDir(), "l")
	testutil.WriteLayout(t, layout, testutil.BuildContent(testutil.ImageSpec{RefName: "hello", Module: testutil.Wasm(t, "hello"), ModulePath: "/bin/hello.wasm"}))
	if _, _, err := d.Pull(context.Background(), "oci:"+layout+":hello", nil); err != nil {
		t.Fatal(err)
	}
	e := New(Options{Complete: WctrCompleter(o, []cli.Candidate{{Value: "wctr"}, {Value: "help"}})})
	t0 := time.Now()
	typ(e, "w\t")
	typ(e, "ru\t")
	typ(e, "--rm\t") // "--r" is --restart and --rm since run has --restart
	typ(e, "hel\t")
	check(t, e, "wctr run --rm hello", 19)
	typ(e, ":\t")
	check(t, e, "wctr run --rm hello:latest ", 27)
	t.Logf("five completions in %v", time.Since(t0))
	e.Reset()
	if ev := typ(e, "ls x\t"); !ev[len(ev)-1].Bell {
		t.Fatal("not wctr: no completion")
	}
	e.Reset()
	o.Client = func(string) (*api.Client, error) { return nil, context.Canceled }
	e2 := New(Options{Complete: WctrCompleter(o, nil)})
	if ev := typ(e2, "wctr logs \t"); !ev[len(ev)-1].Bell {
		t.Fatal("no daemon: bell")
	}
}
