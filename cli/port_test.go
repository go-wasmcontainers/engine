package cli

import (
	"context"
	"encoding/json"
	"io"
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
	"github.com/go-wasmcontainers/engine/portmap"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
)

// portDaemon is a daemon with sockets for its guests and the machine's
// sockets for published ports (wctrd's setup).
func portDaemon(t *testing.T, dir string, host portmap.Host) string {
	t.Helper()
	e := wazero.New(wazero.Options{EngineWASI: true})
	d, err := daemon.New(daemon.Options{Root: filepath.Join(dir, "data"), Containers: container.Options{Executor: e, Ports: host}})
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

func waitListening(t *testing.T, h, name string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(wctr(t, h, "logs", name).stdout, "listening") {
		if time.Now().After(deadline) {
			t.Fatalf("%s never listened", name)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func httpGet(t *testing.T, addr, path string) (string, error) {
	t.Helper()
	cl := &http.Client{Timeout: 10 * time.Second}
	res, err := cl.Get("http://" + addr + path)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	b, err := io.ReadAll(res.Body)
	return string(b), err
}

func TestPublishedPorts(t *testing.T) {
	dir := shortDir(t)
	layout := filepath.Join(dir, "layout")
	testutil.WriteLayout(t, layout, testutil.BuildContent(
		testutil.ImageSpec{RefName: "netprobe", Module: testutil.Wasm(t, "netprobe"), ModulePath: "/bin/netprobe.wasm"},
	))
	h := portDaemon(t, dir, portmap.OS{})
	must(t, wctr(t, h, "pull", "oci:"+layout+":netprobe"), 0)

	// -p with no host port: the system picks one, on 127.0.0.1.
	must(t, wctr(t, h, "run", "-d", "--name", "web", "-p", "127.0.0.1::80", "--expose", "9000", "netprobe", "serve", "80"), 0)
	waitListening(t, h, "web")
	addr := strings.TrimSpace(must(t, wctr(t, h, "port", "web", "80"), 0).stdout)
	if !strings.HasPrefix(addr, "127.0.0.1:") || strings.HasSuffix(addr, ":0") {
		t.Fatalf("port web 80: %q", addr)
	}
	// From the host, through the published port, into the container.
	body, err := httpGet(t, addr, "/x")
	if err != nil || !strings.Contains(body, "hello x from 10.88.") {
		t.Fatalf("GET through the published port: %q %v", body, err)
	}
	if out := must(t, wctr(t, h, "port", "web"), 0).stdout; out != "80/tcp -> "+addr+"\n" {
		t.Fatalf("port web:\n%s", out)
	}
	if r := wctr(t, h, "port", "web", "9000"); r.code == 0 || !strings.Contains(r.stderr, "no public port '9000/tcp'") {
		t.Fatalf("port web 9000: %+v", r)
	}
	if ps := must(t, wctr(t, h, "ps"), 0).stdout; !strings.Contains(ps, addr+"->80/tcp, 9000/tcp") {
		t.Fatalf("ps:\n%s", ps)
	}
	var in []struct {
		Config          struct{ ExposedPorts map[string]struct{} }
		HostConfig      struct{ PortBindings portmap.APIMap }
		NetworkSettings struct{ Ports portmap.APIMap }
	}
	raw := must(t, wctr(t, h, "inspect", "web"), 0).stdout
	if err := json.Unmarshal([]byte(raw), &in); err != nil || len(in) != 1 {
		t.Fatalf("%v\n%s", err, raw)
	}
	got := in[0]
	if _, ok := got.Config.ExposedPorts["9000/tcp"]; !ok || len(got.Config.ExposedPorts) != 2 {
		t.Fatalf("ExposedPorts %v", got.Config.ExposedPorts)
	}
	if b := got.HostConfig.PortBindings["80/tcp"]; len(b) != 1 || b[0].HostIP != "127.0.0.1" || b[0].HostPort != "" {
		t.Fatalf("PortBindings %v", got.HostConfig.PortBindings)
	}
	if b := got.NetworkSettings.Ports["80/tcp"]; len(b) != 1 || b[0].HostIP+":"+b[0].HostPort != addr {
		t.Fatalf("NetworkSettings.Ports %v", got.NetworkSettings.Ports)
	}
	if b, ok := got.NetworkSettings.Ports["9000/tcp"]; !ok || b != nil {
		t.Fatalf("an exposed port is null in NetworkSettings.Ports: %v", got.NetworkSettings.Ports)
	}

	// The same host port for another container: refused, and nothing of it
	// is left listening.
	r := wctr(t, h, "run", "-d", "--name", "web2", "-p", addr+":80", "netprobe", "serve", "80")
	if r.code == 0 || !strings.Contains(r.stderr, "Bind for "+addr+" failed: port is already allocated") {
		t.Fatalf("a taken port: %+v", r)
	}
	// Once the first stops, its port is free again: the second starts.
	must(t, wctr(t, h, "kill", "web"), 0)
	deadline := time.Now().Add(10 * time.Second)
	for {
		l, err := net.Listen("tcp", addr)
		if err == nil {
			l.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s still taken after the container stopped: %v", addr, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	must(t, wctr(t, h, "start", "web2"), 0)
	waitListening(t, h, "web2")
	if body, err := httpGet(t, addr, "/y"); err != nil || !strings.Contains(body, "hello y from") {
		t.Fatalf("web2 on the freed port: %q %v", body, err)
	}
	// A stopped container publishes nothing.
	must(t, wctr(t, h, "kill", "web"), 1) // already stopped
	if r := wctr(t, h, "port", "web", "80"); r.code == 0 {
		t.Fatalf("a stopped container has a port: %+v", r)
	}

	// -P: every exposed port gets one the system picks.
	must(t, wctr(t, h, "run", "-d", "--name", "all", "-P", "--expose", "80", "netprobe", "serve", "80"), 0)
	waitListening(t, h, "all")
	allAddr := strings.TrimSpace(must(t, wctr(t, h, "port", "all", "80/tcp"), 0).stdout)
	if body, err := httpGet(t, allAddr, "/z"); err != nil || !strings.Contains(body, "hello z from") {
		t.Fatalf("-P: %q %v", body, err)
	}

	// Refusals: no network, a bad spec, sctp.
	// A start that fails before the process exists ends a `run --rm` at
	// once, and removes the container: it used to wait for an exit that
	// never came, two minutes per refusal.
	began := time.Now()
	// The CLI exits 125 (the daemon refused the start, as `docker run`
	// does); the container records 128.
	if r := wctr(t, h, "run", "--rm", "--name", "nonet", "--network", "none", "-p", "80", "netprobe", "serve", "80"); r.code != 125 || !strings.Contains(r.stderr, "has no network") {
		t.Fatalf("none: %+v", r)
	}
	if d := time.Since(began); d > 20*time.Second {
		t.Fatalf("a refused start took %s", d)
	}
	if r := wctr(t, h, "inspect", "nonet"); r.code == 0 {
		t.Fatalf("the --rm container of a refused start is still there: %+v", r)
	}
	// Without --rm it stays, exited with the reason (Docker's 128).
	if r := wctr(t, h, "run", "-d", "--name", "kept", "--network", "none", "-p", "80", "netprobe", "serve", "80"); r.code != 125 {
		t.Fatalf("kept: %+v", r)
	}
	if out := must(t, wctr(t, h, "inspect", "kept"), 0).stdout; !strings.Contains(out, `"ExitCode": 128`) || !strings.Contains(out, "has no network") {
		t.Fatalf("inspect kept:\n%s", out)
	}
	for _, bad := range []string{"0", "80/sctp", "x:80", "1-2000"} {
		if r := wctr(t, h, "run", "--rm", "-p", bad, "netprobe", "serve", "80"); r.code != 125 {
			t.Fatalf("-p %s: %+v", bad, r)
		}
	}
	if r := wctr(t, h, "run", "--rm", "--expose", "1:80", "netprobe", "serve", "80"); r.code != 125 {
		t.Fatalf("--expose with a host part: %+v", r)
	}
}

// An engine with nowhere to listen refuses a published port at start, and
// still runs containers that publish none.
func TestPublishedPortsUnavailable(t *testing.T) {
	dir := shortDir(t)
	layout := filepath.Join(dir, "layout")
	testutil.WriteLayout(t, layout, testutil.BuildContent(
		testutil.ImageSpec{RefName: "netprobe", Module: testutil.Wasm(t, "netprobe"), ModulePath: "/bin/netprobe.wasm"},
	))
	h := portDaemon(t, dir, nil)
	must(t, wctr(t, h, "pull", "oci:"+layout+":netprobe"), 0)
	if r := wctr(t, h, "run", "--rm", "-p", "80", "netprobe", "serve", "80"); r.code == 0 || !strings.Contains(r.stderr, "cannot publish ports") {
		t.Fatalf("%+v", r)
	}
	// --expose alone publishes nothing, so it runs.
	must(t, wctr(t, h, "run", "-d", "--name", "quiet", "--expose", "80", "netprobe", "serve", "80"), 0)
	waitListening(t, h, "quiet")
}
