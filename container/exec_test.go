package container

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
	"github.com/go-wasmcontainers/engine/wasi"
)

type safeBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *safeBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// execSetup runs a spinning container on the engine's WASI host.
func execSetup(t *testing.T) (*Manager, *Container) {
	t.Helper()
	f := rootfs.New()
	f.MkdirAll("/usr/bin", 0o755)
	f.MkdirAll("/bin", 0o755)
	f.MkdirAll("/app", 0o755)
	f.WriteFile("/usr/bin/probe.wasm", testutil.Wasm(t, "probe"), 0o755)
	f.WriteFile("/bin/wasixguest.wasm", testutil.Wasm(t, "wasixguest"), 0o755)
	f.WriteFile("/etc-not-a-module", []byte("text"), 0o644)
	f.Freeze()
	e := wazero.New(wazero.Options{HostModules: []wasi.HostModule{wasi.WASIX}})
	t.Cleanup(func() { e.Close(context.Background()) })
	m := NewManager(Options{Executor: e})
	c, err := m.Create(CreateOptions{Name: "/spin", Lower: f, Config: Config{Image: "probe", ImageID: "sha256:x",
		Entrypoint: []string{"probe"}, Cmd: []string{"spin"}, Env: []string{"FROM_IMAGE=1"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), c.ID()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { m.Kill(c.ID()); m.Wait(context.Background(), c.ID(), WaitNotRunning) })
	return m, c
}

// execRun creates and starts an exec, waits for it, and returns its
// output and exit code.
func execRun(t *testing.T, m *Manager, cfg ExecConfig, stdin string) (string, string, ExecInfo) {
	t.Helper()
	x, err := m.CreateExec("spin", cfg)
	if err != nil {
		t.Fatal(err)
	}
	var out, errb safeBuf
	sio := ExecIO{Stdout: &out, Stderr: &errb}
	if stdin != "" {
		sio.Stdin = strings.NewReader(stdin)
	}
	if err := m.StartExec(context.Background(), x, sio); err != nil {
		t.Fatal(err)
	}
	select {
	case <-x.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("exec did not end")
	}
	return out.String(), errb.String(), x.Info()
}

func TestExec(t *testing.T) {
	m, _ := execSetup(t)
	all := func(cfg ExecConfig) ExecConfig {
		cfg.AttachStdout, cfg.AttachStderr = true, true
		return cfg
	}
	out, _, in := execRun(t, m, all(ExecConfig{Cmd: []string{"probe", "args", "a b", "c"}}), "")
	if out != "a b\nc\n" || in.ExitCode != 0 || !in.Ended || in.Running || in.Pid < 2 {
		t.Errorf("args: %q %+v", out, in)
	}
	if _, _, in := execRun(t, m, all(ExecConfig{Cmd: []string{"probe", "exit", "3"}}), ""); in.ExitCode != 3 {
		t.Errorf("exit code %d", in.ExitCode)
	}
	if out, _, _ := execRun(t, m, all(ExecConfig{Cmd: []string{"probe", "cwd"}, WorkingDir: "/app"}), ""); out != "/app\n" {
		t.Errorf("cwd %q", out)
	}
	out, _, _ = execRun(t, m, all(ExecConfig{Cmd: []string{"probe", "env"}, Env: []string{"X=1"}}), "")
	for _, want := range []string{"X=1", "FROM_IMAGE=1", "HOSTNAME=", "PATH="} {
		if !strings.Contains(out, want) {
			t.Errorf("env %q lacks %s", out, want)
		}
	}
	cfg := all(ExecConfig{Cmd: []string{"wasixguest", "child-cat"}, AttachStdin: true})
	if out, _, _ := execRun(t, m, cfg, "through stdin\n"); out != "through stdin\n" {
		t.Errorf("stdin %q", out)
	}
	// Without AttachStdout the output goes nowhere.
	if out, _, in := execRun(t, m, ExecConfig{Cmd: []string{"probe", "args", "x"}}, ""); out != "" || in.ExitCode != 0 {
		t.Errorf("detached output %q", out)
	}
	// A spawn from an exec runs in the container's group.
	if out, _, in := execRun(t, m, all(ExecConfig{Cmd: []string{"wasixguest", "argv"}}), ""); in.ExitCode != 0 || !strings.Contains(out, `"a\nb"`) {
		t.Errorf("spawn from exec: %q %+v", out, in)
	}
}

func TestExecTerminal(t *testing.T) {
	m, _ := execSetup(t)
	tty := ExecConfig{Tty: true, AttachStdin: true, AttachStdout: true, AttachStderr: true, ConsoleSize: [2]uint16{40, 120}}
	cfg := tty
	cfg.Cmd = []string{"wasixguest", "tty"}
	// errno cols rows stdin_tty stdout_tty stderr_tty echo line, CR LF on output.
	if out, _, _ := execRun(t, m, cfg, "\x04"); out != "0 120 40 1 1 1 1 1\r\n" {
		t.Errorf("tty state %q", out)
	}
	// Line discipline: DEL erases, ^C drops the line, CR ends it, ^D ends
	// the input. A line's echo is written before the line reaches the
	// process, so it comes first; later echo and output may interleave.
	cfg = tty
	cfg.Cmd = []string{"wasixguest", "child-cat"}
	out, _, in := execRun(t, m, cfg, "ab\x7fc\rzz\x03ok\r\x04")
	if !strings.HasPrefix(out, "ab\b \bc\r\n") || !strings.Contains(out, "ac\r\n") ||
		!strings.Contains(out, "zz^C\r\n") || strings.Count(out, "ok\r\n") != 2 || in.ExitCode != 0 {
		t.Errorf("ldisc output %q", out)
	}
	if strings.Contains(out, "zz\r\n") {
		t.Errorf("^C did not drop the line: %q", out)
	}
}

func TestExecErrors(t *testing.T) {
	m, c := execSetup(t)
	ctx := context.Background()
	if _, err := m.CreateExec("spin", ExecConfig{}); !errors.Is(err, ErrInvalid) {
		t.Errorf("no command: %v", err)
	}
	if _, err := m.CreateExec("spin", ExecConfig{Cmd: []string{"x"}, User: "nobody"}); !errors.Is(err, ErrInvalid) {
		t.Errorf("user: %v", err)
	}
	if _, err := m.CreateExec("spin", ExecConfig{Cmd: []string{"x"}, Env: []string{"NOEQ"}}); !errors.Is(err, ErrInvalid) {
		t.Errorf("env: %v", err)
	}
	if _, err := m.CreateExec("nope", ExecConfig{Cmd: []string{"x"}}); !errors.Is(err, ErrNotFound) {
		t.Errorf("container: %v", err)
	}
	x, _ := m.CreateExec("spin", ExecConfig{Cmd: []string{"nosuch"}})
	if err := m.StartExec(ctx, x, ExecIO{}); !errors.Is(err, runtime.ErrNotFound) || x.Info().ExitCode != runtime.ExitMissing {
		t.Errorf("missing command: %v %+v", err, x.Info())
	}
	if err := m.StartExec(ctx, x, ExecIO{}); !errors.Is(err, ErrExecStarted) {
		t.Errorf("second start: %v", err)
	}
	x, _ = m.CreateExec("spin", ExecConfig{Cmd: []string{"/etc-not-a-module"}})
	if err := m.StartExec(ctx, x, ExecIO{}); !errors.Is(err, runtime.ErrInvalid) {
		t.Errorf("not a module: %v", err)
	}
	if got, err := m.GetExec(x.ID[:10]); err != nil || got != x {
		t.Errorf("prefix: %v", err)
	}
	if _, err := m.GetExec("ffffffffffff"); !errors.Is(err, ErrNoSuchExec) {
		t.Errorf("unknown: %v", err)
	}
	if err := x.Resize(10, 10); !errors.Is(err, ErrExecNotRunning) {
		t.Errorf("resize without terminal: %v", err)
	}
	// A running exec dies with its container.
	x, _ = m.CreateExec("spin", ExecConfig{Cmd: []string{"probe", "spin"}, Tty: true})
	if err := m.StartExec(ctx, x, ExecIO{}); err != nil {
		t.Fatal(err)
	}
	if err := x.Resize(50, 100); err != nil {
		t.Errorf("resize: %v", err)
	}
	m.Kill(c.ID())
	select {
	case <-x.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("exec outlived its container")
	}
	if in := x.Info(); in.ExitCode != runtime.ExitKilled {
		t.Errorf("killed exec: %+v", in)
	}
	m.Wait(ctx, c.ID(), WaitNotRunning)
	if _, err := m.CreateExec("spin", ExecConfig{Cmd: []string{"probe"}}); !errors.Is(err, ErrNotRunning) {
		t.Errorf("stopped container: %v", err)
	}
}
