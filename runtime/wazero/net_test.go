package wazero

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
)

// background starts a guest and returns its output buffer and process.
func background(t *testing.T, e *Executor, spec runtime.Spec) (*syncBuf, runtime.Process) {
	t.Helper()
	var out syncBuf
	spec.Stdout, spec.Stderr = &out, &out
	// A server lives as long as the test (killed at cleanup), however
	// slow the machine: a wall-clock limit made it vanish mid-test on a
	// loaded CI runner.
	spec.Timeout = 0
	p, err := e.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Kill(); p.Wait() })
	return &out, p
}

func waitFor(t *testing.T, out *syncBuf, s string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(out.String(), s) {
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %q; output so far:\n%s", s, out.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestNetworkGuests runs stock Go wasip1 programs (stealthrocket/net,
// built without the getaddrinfo tag, so names go through the network's
// DNS) on an in-engine network: an HTTP server and its client by
// container name, a UDP echo, a refused dial.
func TestNetworkGuests(t *testing.T) {
	img := lower(t, "netprobe")
	e := New(Options{EngineWASI: true, CompilationCache: testutil.CompilationCache()})
	defer e.Close(context.Background())
	n, err := netdriver.NewLocal(netdriver.DefaultPrefix)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	ctx := context.Background()
	ep := func(name string) netdriver.Endpoint {
		ep, err := n.Attach(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return netdriver.WithDNS(ep)
	}
	spec := func(name string, args ...string) runtime.Spec {
		return runtime.Spec{ID: name, FS: img.Clone(rootfs.Quota{}), Path: "/bin/netprobe.wasm",
			Args: append([]string{"netprobe"}, args...), Net: ep(name), Timeout: time.Minute}
	}

	srv, _ := background(t, e, spec("web", "serve", "8080"))
	waitFor(t, srv, "listening on")
	echo, _ := background(t, e, spec("echo", "udp-echo", "7"))
	waitFor(t, echo, "echoing on")

	r := start(t, e, spec("client", "get", "http://web:8080/class", "3"))
	if r.res.ExitCode != 0 || strings.Count(r.stdout, "hello class from 10.88.") != 3 {
		t.Fatalf("get: %+v", r)
	}
	r = start(t, e, spec("client", "lookup", "web"))
	if r.res.ExitCode != 0 || !strings.Contains(r.stdout, "10.88.0.") {
		t.Fatalf("lookup: %+v", r)
	}
	r = start(t, e, spec("client", "lookup", "nobody"))
	if r.res.ExitCode == 0 || !strings.Contains(r.stderr, "no such host") {
		t.Fatalf("lookup nobody: %+v", r)
	}
	r = start(t, e, spec("client", "udp", "echo:7", "ping"))
	if r.res.ExitCode != 0 || r.stdout != "reply: ping\n" {
		t.Fatalf("udp: %+v", r)
	}
	// Built with -tags getaddrinfo, the guest resolves through
	// sock_getaddrinfo instead of DNS.
	ga := rootfs.New()
	ga.MkdirAll("/bin", 0o755)
	ga.WriteFile("/bin/netprobe.wasm", testutil.WasmTags(t, "netprobe", "getaddrinfo"), 0o755)
	ga.Freeze()
	gs := spec("client", "get", "http://web:8080/getaddrinfo")
	gs.FS = ga.Clone(rootfs.Quota{})
	r = start(t, e, gs)
	if r.res.ExitCode != 0 || !strings.Contains(r.stdout, "hello getaddrinfo from 10.88.") {
		t.Fatalf("get with getaddrinfo: %+v", r)
	}
	gs = spec("client", "dial", "nobody:80")
	gs.FS = ga.Clone(rootfs.Quota{})
	r = start(t, e, gs)
	if r.res.ExitCode != 1 || !strings.Contains(r.stdout, "getaddrinfo") {
		t.Fatalf("getaddrinfo nobody: %+v", r)
	}
	r = start(t, e, spec("client", "dial", "web:9"))
	if r.res.ExitCode != 1 || !strings.Contains(strings.ToLower(r.stdout), "connection refused") {
		t.Fatalf("refused: %+v", r)
	}
	// No network: sockets are not supported.
	none := runtime.Spec{FS: img.Clone(rootfs.Quota{}), Path: "/bin/netprobe.wasm", Args: []string{"netprobe", "dial", "10.88.0.2:8080"}}
	r = start(t, e, none)
	if r.res.ExitCode != 1 || !strings.Contains(strings.ToLower(r.stdout), "not supported") {
		t.Fatalf("no network: %+v", r)
	}
}
