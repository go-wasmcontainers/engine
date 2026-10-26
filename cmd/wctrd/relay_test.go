package main

import (
	"bytes"
	"context"
	"encoding/pem"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/relay/auth"
	v1 "github.com/go-wasmcontainers/relay/gen/relayv1"
	"github.com/go-wasmcontainers/relay/relay"
	"github.com/go-wasmcontainers/relay/server"

	"github.com/go-wasmcontainers/engine/cli"
	"github.com/go-wasmcontainers/engine/internal/testutil"
)

// startRelay serves a relay over TLS and returns its URL, a CA file that
// trusts it, and a join code file (mode 0600) for a Neutron-style class.
func startRelay(t *testing.T, dir string) (string, string, string) {
	t.Helper()
	k, _ := auth.NewKeys()
	r, err := relay.New(relay.Config{Keys: k})
	if err != nil {
		t.Fatal(err)
	}
	tok, _ := auth.RandomToken()
	s, err := server.New(server.Config{Relay: r, AdminToken: tok})
	if err != nil {
		t.Fatal(err)
	}
	pub := httptest.NewTLSServer(s.Public())
	t.Cleanup(pub.Close)
	class, err := r.CreateClass(relay.ClassSpec{Name: "net101", Template: v1.Template_TEMPLATE_NEUTRON_DEFAULT})
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: pub.Certificate().Raw}), 0o644)
	code := filepath.Join(dir, "code")
	os.WriteFile(code, []byte(class.JoinCode+"\n"), 0o600)
	return pub.URL, ca, code
}

func startDaemon(t *testing.T, args ...string) (string, *bytes.Buffer, func()) {
	t.Helper()
	ready := make(chan string, 1)
	cmd := rootCmd(ready)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	select {
	case sock := <-ready:
		return "unix://" + sock, &out, func() { cancel(); <-done }
	case err := <-done:
		cancel()
		t.Fatalf("wctrd: %v\n%s", err, out.String())
	}
	return "", nil, nil
}

func wctr(t *testing.T, host string, want int, args ...string) string {
	t.Helper()
	cmd := cli.RootCmd(cli.Options{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append([]string{"-H", host}, args...))
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	code := 0
	if err := cmd.ExecuteContext(ctx); err != nil {
		code = cli.Report(&out, err)
	}
	if code != want {
		t.Fatalf("wctr %s: exit %d, want %d\n%s", strings.Join(args, " "), code, want, out.String())
	}
	return out.String()
}

// TestRelayClass: wctrd joins a class with a join code (from a file, the
// relay trusted through --relay-ca); its containers serve and reach each
// other by name on the class network; a restart reconnects with the kept
// session, not the code.
func TestRelayClass(t *testing.T) {
	dir := shortDir(t)
	url, ca, code := startRelay(t, dir)
	layout := filepath.Join(dir, "layout")
	testutil.WriteLayout(t, layout, testutil.BuildContent(
		testutil.ImageSpec{RefName: "netprobe", Module: testutil.Wasm(t, "netprobe"), ModulePath: "/bin/netprobe.wasm"},
	))
	data := filepath.Join(dir, "data")
	sock := filepath.Join(dir, "d.sock")
	args := []string{"-H", "unix://" + sock, "--data-root", data, "--relay", url, "--relay-ca", ca, "--relay-join-code-file", code}

	// A world-readable code file is refused.
	os.Chmod(code, 0o644)
	cmd := rootCmd(nil)
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(args)
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "readable by others") {
		t.Fatalf("world-readable code: %v", err)
	}
	os.Chmod(code, 0o600)

	host, log, stop := startDaemon(t, args...)
	if !strings.Contains(log.String(), `joined the class network "class"`) {
		t.Fatal(log.String())
	}
	if st, err := os.Stat(filepath.Join(data, "relay-session")); err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("session file: %v %v", st, err)
	}
	wctr(t, host, 0, "pull", "oci:"+layout+":netprobe")
	if out := wctr(t, host, 0, "network", "ls"); !strings.Contains(out, "class") || !strings.Contains(out, "relay") || !strings.Contains(out, "bridge") {
		t.Fatalf("network ls:\n%s", out)
	}
	wctr(t, host, 0, "run", "-d", "--name", "web", "--network", "class", "netprobe", "serve", "8080")
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(wctr(t, host, 0, "logs", "web"), "listening") {
		if time.Now().After(deadline) {
			t.Fatal("web did not listen")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out := wctr(t, host, 0, "run", "--rm", "--network", "class", "netprobe", "get", "http://web:8080/class"); !strings.Contains(out, "hello class from 10.") {
		t.Fatalf("get: %s", out)
	}
	// The class network is the host's: it cannot be removed.
	wctr(t, host, 1, "network", "rm", "class")
	stop()

	// Restart without the code: the kept session reconnects.
	os.Remove(code)
	_, log, stop = startDaemon(t, args...)
	defer stop()
	if !strings.Contains(log.String(), "joined the class network") || strings.Contains(log.String(), "no longer works") {
		t.Fatal(log.String())
	}
}
