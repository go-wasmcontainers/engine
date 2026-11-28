package relaynet_test

import (
	"context"
	"errors"
	"net/netip"
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
	"github.com/go-wasmcontainers/engine/network"
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
	e := wazero.New(wazero.Options{EngineWASI: true, CompilationCache: testutil.CompilationCache()})
	defer e.Close(context.Background())
	ctx := context.Background()
	run := func(d netdriver.NetworkDriver, name string, out *buf, args ...string) runtime.Process {
		ep, err := d.Attach(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		p, err := e.Start(ctx, runtime.Spec{ID: name, FS: img.Clone(rootfs.Quota{}), Path: "/bin/netprobe.wasm",
			Args: append([]string{"netprobe"}, args...), Net: netdriver.WithDNS(ep), Stdout: out, Stderr: out}) // no wall-clock limit: the server lives for the test
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

type noDNS struct{}

func (noDNS) LookupAddrs(context.Context, string) ([]netip.Addr, error) {
	return nil, errors.New("no lookups in this test")
}

// TestEgressFromARealRelay: a student of a class with an allowlist sees the
// gateway, 443 only, with the operator's name; a student of a class without
// one sees none; and a network.Manager reports both, refusing the first as an
// internal network (it can leave) and accepting the second.
func TestEgressFromARealRelay(t *testing.T) {
	k, err := auth.NewKeys()
	if err != nil {
		t.Fatal(err)
	}
	r, err := relay.New(relay.Config{Keys: k, Egress: relay.EgressConfig{Resolver: noDNS{}, Operator: "Lab IT"}})
	if err != nil {
		t.Fatal(err)
	}
	with, err := r.CreateClass(relay.ClassSpec{Name: "with", RequireID: true, Allowed: []string{"group:students"},
		Egress: []string{"pypi.org:443", "api.example.org:443"}})
	if err != nil {
		t.Fatal(err)
	}
	without, err := r.CreateClass(relay.ClassSpec{Name: "without", Template: v1.Template_TEMPLATE_NEUTRON_DEFAULT})
	if err != nil {
		t.Fatal(err)
	}
	id := &relay.Identity{Issuer: "https://idp", Subject: "ann", Groups: []string{"students"}}
	j, err := r.Join(with.JoinCode, "ann", id)
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
	egressful := relaynet.New(s)
	egressful.SetOrigin("https://relay.example:8443/?token=" + j.Token)
	quiet := relaynet.New(session(t, r, without.JoinCode, "bob"))
	defer egressful.Close()
	defer quiet.Close()

	if !egressful.EgressEnabled() || netdriver.Confined(egressful) {
		t.Error("the egress class is not reported as able to leave")
	}
	if quiet.EgressEnabled() || !netdriver.Confined(quiet) {
		t.Error("the class without egress is not confined")
	}
	st := egressful.EgressState()
	if st.Policy != netdriver.PolicyGateway || st.Gateway == nil || st.Gateway.Operator != "Lab IT" || !st.Gateway.Logs ||
		st.Gateway.Origin != "https://relay.example:8443" || st.Gateway.State != "up" {
		t.Fatalf("%+v %+v", st, st.Gateway)
	}
	if len(st.Allowlist) != 2 || st.Allowlist[0].Port != 443 || st.Allowlist[1].Port != 443 {
		t.Fatalf("allowlist %+v", st.Allowlist)
	}
	if st.Limits.StreamsPerSession == 0 {
		t.Errorf("limits %+v", st.Limits)
	}
	if q := quiet.EgressState(); q.Policy != netdriver.PolicyNone || q.Gateway != nil {
		t.Fatalf("%+v", q)
	}

	m, _, err := network.New(network.Options{Drivers: map[string]netdriver.NetworkDriver{"class": egressful, "quiet": quiet}, DriverName: "class"})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	if n, _ := m.Get("class"); n.Egress.Policy != netdriver.PolicyGateway || n.Egress.Gateway == nil {
		t.Errorf("class: %+v", n.Egress)
	}
	if n, _ := m.Get("quiet"); n.Egress.Policy != netdriver.PolicyNone {
		t.Errorf("quiet: %+v", n.Egress)
	}
	if n, _ := m.Get(network.Bridge); n.Egress.Policy != netdriver.PolicyNone {
		t.Errorf("bridge: %+v", n.Egress)
	}
	// internal: a relay is refused whatever its Welcome says (it is somebody
	// else's machine and would be sent the container's name).
	if _, _, err := network.New(network.Options{Drivers: map[string]netdriver.NetworkDriver{"class": egressful}, Internal: map[string]bool{"class": true}}); err == nil {
		t.Error("an egress relay was accepted on an internal network")
	}
	if _, _, err := network.New(network.Options{Drivers: map[string]netdriver.NetworkDriver{"quiet": quiet}, Internal: map[string]bool{"quiet": true}}); err == nil {
		t.Error("a relay without egress was accepted on an internal network")
	}
}
