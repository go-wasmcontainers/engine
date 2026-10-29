package cli

import (
	"bytes"
	"context"
	"net"
	"net/http"
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
	"github.com/go-wasmcontainers/engine/wasi"
)

// execDaemon serves the engine's WASI host with the WASIX module.
func execDaemon(t *testing.T, dir string) string {
	t.Helper()
	e := wazero.New(wazero.Options{HostModules: []wasi.HostModule{wasi.WASIX}})
	d, err := daemon.New(daemon.Options{Root: filepath.Join(dir, "data"), Containers: container.Options{Executor: e}})
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

func wctrIn(t *testing.T, host, stdin string, args ...string) result {
	t.Helper()
	cmd := RootCmd(Options{})
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetIn(strings.NewReader(stdin))
	cmd.SetArgs(append([]string{"-H", host}, args...))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code := 0
	if err := cmd.ExecuteContext(ctx); err != nil {
		code = Report(&errb, err)
	}
	return result{out.String(), errb.String(), code}
}

func TestExecCommand(t *testing.T) {
	dir := shortDir(t)
	layout := filepath.Join(dir, "layout")
	testutil.WriteLayout(t, layout, testutil.BuildContent(testutil.ImageSpec{
		RefName: "probe", Module: testutil.Wasm(t, "probe"), ModulePath: "/bin/probe.wasm",
		Files: map[string]string{"bin/wasixguest.wasm": string(testutil.Wasm(t, "wasixguest"))},
		Cmd:   []string{"spin"},
	}))
	h := execDaemon(t, dir)
	must(t, wctr(t, h, "pull", "oci:"+layout+":probe"), 0)
	must(t, wctr(t, h, "run", "-d", "--name", "c", "probe"), 0)

	if r := must(t, wctr(t, h, "exec", "c", "probe", "args", "a b", "c"), 0); r.stdout != "a b\nc\n" {
		t.Errorf("exec args: %+v", r)
	}
	if r := wctr(t, h, "exec", "-e", "X=1", "-w", "/bin", "c", "probe", "exit", "4"); r.code != 4 {
		t.Errorf("exit status: %+v", r)
	}
	if r := must(t, wctr(t, h, "exec", "-w", "/bin", "c", "probe", "cwd"), 0); r.stdout != "/bin\n" {
		t.Errorf("workdir: %+v", r)
	}
	if r := must(t, wctrIn(t, h, "piped in\n", "exec", "-i", "c", "wasixguest", "child-cat"), 0); r.stdout != "piped in\n" {
		t.Errorf("stdin: %+v", r)
	}
	if r := must(t, wctrIn(t, h, "hi\r\x04", "exec", "-it", "c", "wasixguest", "child-cat"), 0); r.stdout != "hi\r\nhi\r\n" {
		t.Errorf("terminal: %q", r.stdout)
	}
	must(t, wctr(t, h, "exec", "-d", "c", "probe", "exit", "0"), 0)
	if r := wctr(t, h, "exec", "c", "nosuch"); r.code == 0 || !strings.Contains(r.stderr, "not found") {
		t.Errorf("missing command: %+v", r)
	}
	if r := wctr(t, h, "exec", "-u", "nobody", "c", "probe"); r.code == 0 || !strings.Contains(r.stderr, "no users") {
		t.Errorf("user: %+v", r)
	}
	if r := wctr(t, h, "exec", "nope", "probe"); r.code == 0 || !strings.Contains(r.stderr, "No such container") && !strings.Contains(r.stderr, "no such container") {
		t.Errorf("unknown container: %+v", r)
	}

	// The API directly: inspect, resize, a second start.
	c, err := api.NewClient(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	id, err := c.ExecCreate(ctx, "c", api.ExecCreateRequest{Cmd: []string{"probe", "exit", "7"}})
	if err != nil {
		t.Fatal(err)
	}
	in, err := c.ExecInspect(ctx, id)
	if err != nil || in.ExitCode != nil || in.Running || in.ProcessConfig.Entrypoint != "probe" {
		t.Fatalf("before start: %+v %v", in, err)
	}
	if _, err := c.ExecStart(ctx, id, api.ExecStartRequest{Detach: true}, nil); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(30 * time.Second)
	for {
		in, err = c.ExecInspect(ctx, id)
		if err != nil || in.ExitCode != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if in.ExitCode == nil || *in.ExitCode != 7 || in.Pid < 2 {
		t.Errorf("after: %+v %v", in, err)
	}
	if _, err := c.ExecStart(ctx, id, api.ExecStartRequest{Detach: true}, nil); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Errorf("second start: %v", err)
	}
	if err := c.ExecResize(ctx, id, 10, 10); err == nil {
		t.Error("resized an ended exec without a terminal")
	}
	if _, err := c.ExecInspect(ctx, "0000000000000000"); !api.IsNotFound(err) {
		t.Errorf("unknown exec: %v", err)
	}
	must(t, wctr(t, h, "rm", "-f", "c"), 0)
}
