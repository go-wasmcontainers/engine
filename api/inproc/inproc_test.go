package inproc_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
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

func TestStreaming(t *testing.T) {
	release := make(chan struct{})
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/flush":
			w.Header().Set("X-Early", "1")
			w.WriteHeader(201)
			http.NewResponseController(w).Flush()
			<-release
			io.WriteString(w, "late")
		case "/nothing":
		case "/cancel":
			w.WriteHeader(200)
			<-r.Context().Done()
		case "/echo":
			b, _ := io.ReadAll(r.Body)
			w.Write(b)
		}
	})
	c := &http.Client{Transport: &inproc.Transport{Handler: h}}
	resp, err := c.Get("http://x/flush")
	if err != nil || resp.StatusCode != 201 || resp.Header.Get("X-Early") != "1" {
		t.Fatal(resp, err)
	}
	close(release)
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "late" {
		t.Fatal(string(b))
	}
	if resp, err = c.Get("http://x/nothing"); err != nil || resp.StatusCode != 200 {
		t.Fatal(resp, err)
	}
	resp.Body.Close()
	resp, err = c.Post("http://x/echo", "text/plain", strings.NewReader("ping"))
	if b, _ := io.ReadAll(resp.Body); err != nil || string(b) != "ping" {
		t.Fatal(string(b), err)
	}
	// Closing the body cancels the handler's context.
	resp, _ = c.Get("http://x/cancel")
	resp.Body.Close()
	// A request whose context ends before the header is refused.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	block := &http.Client{Transport: &inproc.Transport{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() })}}
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://x/", nil)
	if _, err := block.Do(req); err == nil {
		t.Fatal("no error after the context ended")
	}
}

// TestCLI runs the wctr command tree against an in-process engine through
// the transport, the way the playground does: run streams logs and the
// exit code comes back.
func TestCLI(t *testing.T) {
	e := wazero.New(wazero.Options{EngineWASI: true, CompilationCache: testutil.CompilationCache()})
	defer e.Close(context.Background())
	d, err := daemon.New(daemon.Options{Backend: image.NewMemoryBackend(0), Containers: container.Options{Executor: e}})
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{Transport: &inproc.Transport{Handler: server.New(d, server.Info{Version: "t"})}}
	client := func(string) (*api.Client, error) { return api.NewClientWith(hc, "http://engine"), nil }
	layout := filepath.Join(t.TempDir(), "l")
	testutil.WriteLayout(t, layout, testutil.BuildContent(
		testutil.ImageSpec{RefName: "probe", Module: testutil.Wasm(t, "probe"), ModulePath: "/bin/probe.wasm"}))
	run := func(args ...string) (string, int) {
		cmd := cli.RootCmd(cli.Options{Client: client, DefaultHost: "inproc://"})
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		code := 0
		if err := cmd.Execute(); err != nil {
			code = cli.Report(&out, err)
		}
		return out.String(), code
	}
	if out, code := run("pull", "oci:"+layout+":probe"); code != 0 {
		t.Fatal(out)
	}
	if out, code := run("run", "--rm", "probe", "args", "a", "b"); code != 0 || out != "a\nb\n" {
		t.Fatalf("%d %q", code, out)
	}
	if out, code := run("run", "--name", "x", "probe", "exit", "9"); code != 9 || out != "bye\n" {
		t.Fatalf("%d %q", code, out)
	}
	if out, code := run("ps", "-a"); code != 0 || !strings.Contains(out, "Exited (9)") {
		t.Fatal(out)
	}
}
