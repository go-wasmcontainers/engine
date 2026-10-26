package relaynet_test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-wasmcontainers/relay/auth"
	"github.com/go-wasmcontainers/relay/driver"
	v1 "github.com/go-wasmcontainers/relay/gen/relayv1"
	"github.com/go-wasmcontainers/relay/relay"

	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/netdriver/relaynet"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
)

type buf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (b *buf) Write(p []byte) (int, error) { b.mu.Lock(); defer b.mu.Unlock(); return b.b.Write(p) }
func (b *buf) String() string              { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

// session joins a class of an in-process relay and connects over a pipe,
// as relay/relay_test.go does.
func session(t *testing.T, r *relay.Relay, code, name string) *driver.Session {
	t.Helper()
	j, err := r.Join(code, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	cl, err := r.Authenticate(j.Token)
	if err != nil {
		t.Fatal(err)
	}
	a, b := relay.Pipe()
	go r.Serve(context.Background(), b, cl)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, err := driver.Dial(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestClassNetwork runs two engines' guests on one class network: alice's
// HTTP server, bob's client reaching it by its class name, through the
// relay and its firewall (the Neutron template: same group, allowed).
func TestClassNetwork(t *testing.T) {
	k, err := auth.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	r, err := relay.New(relay.Config{Keys: k})
	if err != nil {
		t.Fatal(err)
	}
	class, err := r.CreateClass(relay.ClassSpec{Name: "net101", Template: v1.Template_TEMPLATE_NEUTRON_DEFAULT})
	if err != nil {
		t.Fatal(err)
	}
	alice := relaynet.New(session(t, r, class.JoinCode, "alice"))
	bob := relaynet.New(session(t, r, class.JoinCode, "bob"))
	defer alice.Close()
	defer bob.Close()

	img := rootfs.New()
	img.MkdirAll("/bin", 0o755)
	img.WriteFile("/bin/netprobe.wasm", testutil.Wasm(t, "netprobe"), 0o755)
	img.Freeze()
	e := wazero.New(wazero.Options{EngineWASI: true})
	defer e.Close(context.Background())
	ctx := context.Background()
	run := func(d netdriver.NetworkDriver, name string, out *buf, args ...string) runtime.Process {
		ep, err := d.Attach(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		p, err := e.Start(ctx, runtime.Spec{ID: name, FS: img.Clone(rootfs.Quota{}), Path: "/bin/netprobe.wasm",
			Args: append([]string{"netprobe"}, args...), Net: netdriver.WithDNS(ep), Stdout: out, Stderr: out, Timeout: time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		return p
	}
	var srvOut buf
	srv := run(alice, "web", &srvOut, "serve", "8080")
	defer func() { srv.Kill(); srv.Wait() }()
	for deadline := time.Now().Add(20 * time.Second); !strings.Contains(srvOut.String(), "listening"); {
		if time.Now().After(deadline) {
			t.Fatalf("server: %s", srvOut.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	var cliOut buf
	host := "web.alice" // <container>.<student>, through the network's DNS
	res := run(bob, "client", &cliOut, "get", "http://"+host+":8080/bob", "2").Wait()
	if res.ExitCode != 0 || strings.Count(cliOut.String(), "hello bob from ") != 2 {
		t.Fatalf("%s: exit %d: %s", host, res.ExitCode, cliOut.String())
	}
}
