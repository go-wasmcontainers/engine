package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/api/server"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/daemon"
	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
)

// shortDir is a temp dir short enough for a unix socket path (macOS caps
// sun_path at 104 bytes, and t.TempDir() is long there).
func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "wc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// startDaemon serves a fresh engine in-process and returns its host.
func startDaemon(t *testing.T, copt container.Options) string {
	t.Helper()
	dir := shortDir(t)
	e := wazero.New(wazero.Options{})
	copt.Executor = e
	d, err := daemon.New(daemon.Options{Root: filepath.Join(dir, "data"), Containers: copt})
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

// testLayout writes the demo images to an OCI layout and returns its dir.
func testLayout(t *testing.T) string {
	t.Helper()
	c := testutil.BuildContent(
		testutil.ImageSpec{RefName: "hello", Module: testutil.Wasm(t, "hello"), ModulePath: "/bin/hello.wasm"},
		testutil.ImageSpec{RefName: "greet", Module: testutil.Wasm(t, "greet"), Artifact: true, Index: true},
		testutil.ImageSpec{RefName: "probe", Module: testutil.Wasm(t, "probe"), ModulePath: "/bin/probe.wasm",
			Files: map[string]string{"etc/motd": "welcome\n"}, Env: []string{"FROM_IMAGE=1"}},
	)
	dir := filepath.Join(shortDir(t), "layout")
	testutil.WriteLayout(t, dir, c)
	return dir
}

type result struct {
	stdout, stderr string
	code           int
}

// docker runs the CLI in-process against host.
func docker(t *testing.T, host string, args ...string) result {
	t.Helper()
	cmd := RootCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(append([]string{"-H", host}, args...))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code := 0
	if err := cmd.ExecuteContext(ctx); err != nil {
		code = report(&errb, err)
	}
	return result{out.String(), errb.String(), code}
}

func must(t *testing.T, r result, code int) result {
	t.Helper()
	if r.code != code {
		t.Fatalf("exit %d, want %d\nstdout: %s\nstderr: %s", r.code, code, r.stdout, r.stderr)
	}
	return r
}

func TestDemo(t *testing.T) {
	host := startDaemon(t, container.Options{})
	layout := testLayout(t)

	r := must(t, docker(t, host, "pull", "oci:"+layout+":hello"), 0)
	if !strings.Contains(r.stdout, "Status: Downloaded newer image for hello:latest") || !strings.Contains(r.stdout, "Pull complete") {
		t.Fatal(r.stdout)
	}
	r = must(t, docker(t, host, "pull", "oci:"+layout+":hello"), 0)
	if !strings.Contains(r.stdout, "Image is up to date") {
		t.Fatal(r.stdout)
	}
	must(t, docker(t, host, "pull", "oci:"+layout+":greet"), 0)
	must(t, docker(t, host, "pull", "oci:"+layout+":probe"), 0)

	r = must(t, docker(t, host, "images"), 0)
	for _, want := range []string{"REPOSITORY", "hello", "greet", "probe", "latest"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("images lacks %q:\n%s", want, r.stdout)
		}
	}
	if q := must(t, docker(t, host, "images", "-q"), 0); len(strings.Fields(q.stdout)) != 3 {
		t.Fatal(q.stdout)
	}

	r = must(t, docker(t, host, "run", "--rm", "hello"), 0)
	if r.stdout != "Hello from a WebAssembly container!\n" {
		t.Fatalf("%q", r.stdout)
	}
	r = must(t, docker(t, host, "run", "--rm", "-e", "NAME=lab", "greet"), 0)
	if r.stdout != "Hello, lab!\n" {
		t.Fatalf("%q", r.stdout)
	}
	t.Setenv("NAME", "from-shell")
	if r = must(t, docker(t, host, "run", "--rm", "-e", "NAME", "-e", "UNSET_VAR_X", "greet"), 0); r.stdout != "Hello, from-shell!\n" {
		t.Fatalf("%q", r.stdout)
	}

	// Exit codes propagate; the container stays for ps/logs/inspect.
	r = must(t, docker(t, host, "run", "--name", "p", "probe", "exit", "3"), 3)
	if r.stderr != "bye\n" {
		t.Fatalf("%q", r.stderr)
	}
	r = must(t, docker(t, host, "ps", "-a"), 0)
	if !strings.Contains(r.stdout, "Exited (3)") || !strings.Contains(r.stdout, `"/bin/probe.wasm exi…"`) || !strings.Contains(r.stdout, " p\n") {
		t.Fatalf("ps -a:\n%s", r.stdout)
	}
	if strings.Contains(r.stdout, "hello") {
		t.Fatalf("--rm left a container:\n%s", r.stdout)
	}
	if r = must(t, docker(t, host, "ps"), 0); strings.Count(r.stdout, "\n") != 1 {
		t.Fatalf("ps shows stopped containers:\n%s", r.stdout)
	}
	r = must(t, docker(t, host, "logs", "p"), 0)
	if r.stderr != "bye\n" || r.stdout != "" {
		t.Fatalf("logs: %q %q", r.stdout, r.stderr)
	}
	r = must(t, docker(t, host, "inspect", "p"), 0)
	var insp []api.ContainerInspect
	if err := json.Unmarshal([]byte(r.stdout), &insp); err != nil || len(insp) != 1 {
		t.Fatal(err, r.stdout)
	}
	if insp[0].State.ExitCode != 3 || insp[0].State.Status != "exited" || insp[0].Name != "/p" || insp[0].Path != "/bin/probe.wasm" {
		t.Fatalf("%+v", insp[0])
	}
	// Image env and image files are there; -w and argv work.
	r = must(t, docker(t, host, "run", "--rm", "-w", "/etc", "probe", "cat", "motd"), 0)
	if r.stdout != "welcome\n" {
		t.Fatalf("%q", r.stdout)
	}
	r = must(t, docker(t, host, "run", "--rm", "probe", "env"), 0)
	if !strings.Contains(r.stdout, "FROM_IMAGE=1\n") {
		t.Fatalf("%q", r.stdout)
	}
	must(t, docker(t, host, "rm", "p"), 0)
	if r = must(t, docker(t, host, "ps", "-a", "-q"), 0); r.stdout != "" {
		t.Fatalf("%q", r.stdout)
	}

	// Detached, killed, force-removed.
	r = must(t, docker(t, host, "run", "-d", "--name", "spin", "probe", "spin"), 0)
	id := strings.TrimSpace(r.stdout)
	if len(id) != 64 {
		t.Fatal(id)
	}
	r = must(t, docker(t, host, "rm", "spin"), 1)
	if !strings.Contains(r.stderr, "running") {
		t.Fatal(r.stderr)
	}
	must(t, docker(t, host, "kill", id[:12]), 0)
	must(t, docker(t, host, "rm", "-f", "spin"), 0)

	// Images: in use, removed, the second removal fails.
	must(t, docker(t, host, "run", "--name", "keep", "hello"), 0)
	r = must(t, docker(t, host, "rmi", "hello"), 1)
	if !strings.Contains(r.stderr, "is using it") {
		t.Fatal(r.stderr)
	}
	must(t, docker(t, host, "rm", "keep"), 0)
	r = must(t, docker(t, host, "rmi", "hello"), 0)
	if !strings.Contains(r.stdout, "Untagged: hello:latest") || !strings.Contains(r.stdout, "Deleted: sha256:") {
		t.Fatal(r.stdout)
	}
	must(t, docker(t, host, "rmi", "hello"), 1)

	// run pulls what is missing.
	r = must(t, docker(t, host, "run", "--rm", "oci:"+layout+":hello"), 0)
	if !strings.Contains(r.stderr, "Unable to find image") || r.stdout != "Hello from a WebAssembly container!\n" {
		t.Fatalf("%+v", r)
	}

	// Image inspect through the fallback.
	r = must(t, docker(t, host, "inspect", "greet"), 0)
	if !strings.Contains(r.stdout, `"Os": "wasip1"`) || !strings.Contains(r.stdout, "/app.wasm") {
		t.Fatal(r.stdout)
	}
	r = must(t, docker(t, host, "inspect", "nothing-here"), 1)
	if !strings.Contains(r.stderr, "No such object") {
		t.Fatal(r.stderr)
	}

	r = must(t, docker(t, host, "version"), 0)
	if !strings.Contains(r.stdout, "API version: 1.47") || !strings.Contains(r.stdout, "Server:") {
		t.Fatal(r.stdout)
	}
}

func TestRunErrors(t *testing.T) {
	host := startDaemon(t, container.Options{Timeout: 500 * time.Millisecond})
	layout := testLayout(t)
	must(t, docker(t, host, "pull", "oci:"+layout+":probe"), 0)

	// A command that is not in the image: 127, like docker.
	r := docker(t, host, "run", "--rm", "--entrypoint", "/nope.wasm", "probe")
	if r.code != 127 || !strings.Contains(r.stderr, "module not found") {
		t.Fatalf("%+v", r)
	}
	// A file that is not a module: 126.
	r = docker(t, host, "run", "--rm", "--entrypoint", "/etc/motd", "probe")
	if r.code != 126 {
		t.Fatalf("%+v", r)
	}
	// The wall-clock limit ends a spinning container with 137.
	if r = docker(t, host, "run", "--rm", "probe", "spin"); r.code != 137 {
		t.Fatalf("%+v", r)
	}
	// A memory limit from the command line.
	if r = docker(t, host, "run", "--rm", "-m", "32m", "probe", "hog"); r.code == 0 {
		t.Fatalf("%+v", r)
	}
	if r = docker(t, host, "run", "-m", "lots", "probe"); r.code != 125 {
		t.Fatalf("%+v", r)
	}
	// Names: invalid, conflicting.
	if r = docker(t, host, "run", "--name", "bad name", "probe", "args"); r.code != 125 || !strings.Contains(r.stderr, "invalid container name") {
		t.Fatalf("%+v", r)
	}
	must(t, docker(t, host, "run", "--name", "dup", "probe", "args"), 0)
	if r = docker(t, host, "run", "--name", "dup", "probe", "args"); r.code != 125 || !strings.Contains(r.stderr, "already in use") {
		t.Fatalf("%+v", r)
	}
	// Pulling something that is not there.
	if r = docker(t, host, "run", "oci:"+layout+":absent"); r.code != 125 {
		t.Fatalf("%+v", r)
	}
	if r = docker(t, host, "pull", "oci:/no/such/dir:x"); r.code != 1 || !strings.Contains(r.stderr, "Error response from daemon") {
		t.Fatalf("%+v", r)
	}
	// Bad host / no daemon.
	if r = docker(t, "tcp://127.0.0.1:2375", "ps"); r.code != 1 || !strings.Contains(r.stderr, "unix") {
		t.Fatalf("%+v", r)
	}
	if r = docker(t, "unix:///nonexistent/d.sock", "run", "probe"); r.code != 125 {
		t.Fatalf("%+v", r)
	}
	if r = docker(t, host, "logs", "nope"); r.code != 1 {
		t.Fatalf("%+v", r)
	}
	if r = docker(t, host, "kill", "dup"); r.code != 1 {
		t.Fatalf("%+v", r)
	}
	if r = docker(t, host, "rmi", "sha256:0000"); r.code != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestRegistryPull(t *testing.T) {
	host := startDaemon(t, container.Options{})
	c := testutil.BuildContent(testutil.ImageSpec{RefName: "v1", Module: testutil.Wasm(t, "hello"), Index: true})
	reg := testutil.NewRegistry("lab/hello", c)
	reg.RequireToken = true
	defer reg.Close()
	ref := reg.Host() + "/lab/hello:v1"
	r := must(t, docker(t, host, "pull", ref), 0)
	if !strings.Contains(r.stdout, "Downloaded newer image for "+ref) {
		t.Fatal(r.stdout)
	}
	r = must(t, docker(t, host, "run", "--rm", ref), 0)
	if r.stdout != "Hello from a WebAssembly container!\n" {
		t.Fatal(r.stdout)
	}
	r = must(t, docker(t, host, "images"), 0)
	if !strings.Contains(r.stdout, reg.Host()+"/lab/hello") {
		t.Fatal(r.stdout)
	}
}

func TestHelpers(t *testing.T) {
	if repo, tag := splitRepoTag("localhost:5000/x"); repo != "localhost:5000/x" || tag != "<none>" {
		t.Fatal(repo, tag)
	}
	if quoteCommand(strings.Repeat("x", 30)) != `"`+strings.Repeat("x", 19)+`…"` {
		t.Fatal("quote")
	}
	wd, _ := os.Getwd()
	if got := absLayout("oci:./l:hello"); got != "oci:"+filepath.Join(wd, "l")+":hello" {
		t.Fatal(got)
	}
	if got := absLayout("oci:rel"); got != "oci:"+filepath.Join(wd, "rel") {
		t.Fatal(got)
	}
	if absLayout("hello") != "hello" || absLayout("oci:") != "oci:" {
		t.Fatal("non-layout refs changed")
	}
	if shortID("sha256:abc") != "abc" {
		t.Fatal("short")
	}
	var b bytes.Buffer
	if report(&b, &exitError{code: 4}) != 4 || b.Len() != 0 {
		t.Fatal("silent exit")
	}
	if (&exitError{code: 4}).Error() != "exit status 4" {
		t.Fatal("exit text")
	}
	// main() through osExit.
	code := -1
	osExit = func(c int) { code = c }
	defer func() { osExit = os.Exit }()
	os.Args = []string{"docker", "-H", "unix:///nonexistent/x.sock", "ps"}
	main()
	if code != 1 {
		t.Fatal(code)
	}
}
