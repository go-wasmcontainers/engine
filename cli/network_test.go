package cli

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api/server"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/daemon"
	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
	"github.com/go-wasmcontainers/engine/state"
)

// netDaemon is startDaemon with the engine's WASI host (sockets) and a
// state store (networks are kept).
func netDaemon(t *testing.T, dir string) string {
	t.Helper()
	e := wazero.New(wazero.Options{EngineWASI: true, CompilationCache: testutil.CompilationCache()})
	st, err := state.OpenDir(filepath.Join(dir, "containers"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.New(daemon.Options{Root: filepath.Join(dir, "data"), Containers: container.Options{Executor: e, Store: st}})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: server.New(d, server.Info{Version: "test"})}
	go srv.Serve(l)
	t.Cleanup(func() {
		d.Close(context.Background())
		srv.Close()
		e.Close(context.Background())
	})
	return "unix://" + sock
}

func TestNetworks(t *testing.T) {
	dir := shortDir(t)
	layout := filepath.Join(dir, "layout")
	testutil.WriteLayout(t, layout, testutil.BuildContent(
		testutil.ImageSpec{RefName: "netprobe", Module: testutil.Wasm(t, "netprobe"), ModulePath: "/bin/netprobe.wasm"},
	))
	h := netDaemon(t, dir)
	must(t, wctr(t, h, "pull", "oci:"+layout+":netprobe"), 0)

	ls := must(t, wctr(t, h, "network", "ls"), 0).stdout
	if !strings.Contains(ls, "bridge") || !strings.Contains(ls, "10.88.0.0/16") || !strings.Contains(ls, "none") {
		t.Fatalf("network ls:\n%s", ls)
	}
	must(t, wctr(t, h, "network", "create", "lab"), 0)
	if r := wctr(t, h, "network", "create", "lab"); r.code != 1 || !strings.Contains(r.stderr, "already exists") {
		t.Fatalf("%+v", r)
	}
	// Containers reach each other by name, on the default network and on
	// a created one.
	for _, nw := range []string{"", "lab"} {
		args := []string{"run", "-d", "--name", "web" + nw}
		if nw != "" {
			args = append(args, "--network", nw)
		}
		must(t, wctr(t, h, append(args, "netprobe", "serve", "80")...), 0)
		deadline := time.Now().Add(20 * time.Second)
		for !strings.Contains(wctr(t, h, "logs", "web"+nw).stdout, "listening") {
			if time.Now().After(deadline) {
				t.Fatal("no listener")
			}
			time.Sleep(20 * time.Millisecond)
		}
		args = []string{"run", "--rm"}
		if nw != "" {
			args = append(args, "--network", nw)
		}
		if r := must(t, wctr(t, h, append(args, "netprobe", "get", "http://web"+nw+"/x")...), 0); !strings.Contains(r.stdout, "hello x from 10.") {
			t.Fatalf("%q: %+v", nw, r)
		}
	}
	// inspect: the address of a running container, on both objects.
	raw := must(t, wctr(t, h, "network", "inspect", "lab"), 0).stdout
	var nets []struct {
		Containers map[string]struct{ Name, IPv4Address string }
		IPAM       struct{ Config []map[string]string }
	}
	if err := json.Unmarshal([]byte(raw), &nets); err != nil || len(nets) != 1 || len(nets[0].Containers) != 1 || nets[0].IPAM.Config[0]["Subnet"] != "10.89.0.0/16" {
		t.Fatalf("inspect lab: %v\n%s", err, raw)
	}
	for _, c := range nets[0].Containers {
		if c.Name != "weblab" || !strings.HasPrefix(c.IPv4Address, "10.89.0.") {
			t.Fatalf("%+v", c)
		}
	}
	if r := must(t, wctr(t, h, "inspect", "weblab"), 0); !strings.Contains(r.stdout, `"NetworkMode": "lab"`) || !strings.Contains(r.stdout, `"IPAddress": "10.89.0.`) {
		t.Fatalf("inspect weblab:\n%s", r.stdout)
	}
	// Refusals: in use, builtin, host, unknown; none has no sockets.
	if r := wctr(t, h, "network", "rm", "lab"); r.code != 1 || !strings.Contains(r.stderr, "active endpoints") {
		t.Fatalf("rm in use: %+v", r)
	}
	if r := wctr(t, h, "network", "rm", "bridge"); r.code != 1 || !strings.Contains(r.stderr, "pre-defined") {
		t.Fatalf("rm bridge: %+v", r)
	}
	if r := wctr(t, h, "run", "--rm", "--network", "host", "netprobe", "lookup", "x"); r.code != 125 {
		t.Fatalf("host: %+v", r)
	}
	if r := wctr(t, h, "run", "--rm", "--network", "nowhere", "netprobe", "lookup", "x"); r.code != 125 || !strings.Contains(r.stderr, "not found") {
		t.Fatalf("unknown network: %+v", r)
	}
	if r := wctr(t, h, "run", "--rm", "--network", "none", "netprobe", "dial", "10.88.0.2:80"); r.code != 1 || !strings.Contains(strings.ToLower(r.stdout), "not supported") {
		t.Fatalf("none: %+v", r)
	}
	must(t, wctr(t, h, "rm", "-f", "weblab"), 0)
	must(t, wctr(t, h, "network", "rm", "lab"), 0)
}
