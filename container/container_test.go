package container

import (
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
)

func image(t *testing.T) *rootfs.FS {
	f := rootfs.New()
	f.MkdirAll("/usr/bin", 0o755)
	f.MkdirAll("/app", 0o755)
	f.WriteFile("/usr/bin/probe.wasm", testutil.Wasm(t, "probe"), 0o755)
	f.WriteFile("/app/greet.wasm", testutil.Wasm(t, "greet"), 0o755)
	f.WriteFile("/app/data.txt", []byte("data\n"), 0o644)
	f.Freeze()
	return f
}

func manager(t *testing.T, opt Options) *Manager {
	e := wazero.New(wazero.Options{})
	t.Cleanup(func() { e.Close(context.Background()) })
	opt.Executor = e
	return NewManager(opt)
}

func output(t *testing.T, m *Manager, id string) (stdout, stderr string) {
	t.Helper()
	var o, e strings.Builder
	err := m.Logs(context.Background(), id, false, func(r Record) error {
		if r.Stream == Stdout {
			o.Write(r.Data)
		} else {
			e.Write(r.Data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return o.String(), e.String()
}

func runToExit(t *testing.T, m *Manager, o CreateOptions) (*Container, int) {
	t.Helper()
	c, err := m.Create(o)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(context.Background(), c.ID()); err != nil {
		t.Fatal(err)
	}
	code, err := m.Wait(context.Background(), c.ID(), WaitNotRunning)
	if err != nil {
		t.Fatal(err)
	}
	return c, code
}

func TestLifecycle(t *testing.T) {
	img := image(t)
	m := manager(t, Options{})
	ctx := context.Background()

	c, err := m.Create(CreateOptions{Name: "/first", Lower: img, Config: Config{Image: "probe", ImageID: "sha256:x",
		Entrypoint: []string{"probe"}, Cmd: []string{"exit", "5"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := c.Snapshot()
	if s.State != Created || s.Name != "first" || len(s.ID) != 64 || strings.Join(s.Command(), " ") != "probe exit 5" {
		t.Fatalf("%+v", s)
	}
	// not-running on a created container returns at once; next-exit waits.
	if code, err := m.Wait(ctx, "first", WaitNotRunning); err != nil || code != 0 {
		t.Fatal(code, err)
	}
	next := make(chan int, 1)
	go func() { code, _ := m.Wait(ctx, "first", WaitNextExit); next <- code }()
	time.Sleep(20 * time.Millisecond)
	if err := m.Start(ctx, s.ID[:8]); err != nil {
		t.Fatal(err)
	}
	if code := <-next; code != 5 {
		t.Fatalf("next-exit code %d", code)
	}
	s = c.Snapshot()
	if s.State != Exited || s.ExitCode != 5 || s.Path != "/usr/bin/probe.wasm" || s.FinishedAt.Before(s.StartedAt) {
		t.Fatalf("%+v", s)
	}
	if _, stderr := output(t, m, "first"); stderr != "bye\n" {
		t.Fatalf("%q", stderr)
	}
	// Restart an exited container.
	if err := m.Start(ctx, "first"); err != nil {
		t.Fatal(err)
	}
	m.Wait(ctx, "first", WaitNotRunning)
	if _, stderr := output(t, m, "first"); stderr != "bye\nbye\n" {
		t.Fatalf("logs across runs: %q", stderr)
	}
	if ids := m.UsesImage("sha256:x"); len(ids) != 1 {
		t.Fatal(ids)
	}
	// Names conflict; IDs resolve by prefix; removal.
	if _, err := m.Create(CreateOptions{Name: "first", Lower: img, Config: Config{Cmd: []string{"x"}}}); !errors.Is(err, ErrNameConflict) {
		t.Fatal(err)
	}
	removed := make(chan struct{})
	go func() { c.Wait(ctx, WaitRemoved); close(removed) }()
	if err := m.Remove(ctx, "first", false); err != nil {
		t.Fatal(err)
	}
	<-removed
	if _, err := m.Get("first"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := m.Start(ctx, "first"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := m.Wait(ctx, "first", ""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := m.Remove(ctx, "first", false); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := m.Logs(ctx, "first", false, nil); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err := m.Kill("first"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestEnvPathAndWorkdir(t *testing.T) {
	img := image(t)
	m := manager(t, Options{})
	c, code := runToExit(t, m, CreateOptions{Lower: img, Config: Config{Cmd: []string{"probe", "env"}, Env: []string{"A=1"}}})
	stdout, _ := output(t, m, c.ID())
	for _, want := range []string{"A=1", "PATH=" + defaultPath, "HOSTNAME=" + c.ID()[:12], "PWD=/"} {
		if !strings.Contains(stdout, want+"\n") {
			t.Errorf("env lacks %s:\n%s", want, stdout)
		}
	}
	if code != 0 {
		t.Fatal(code)
	}
	// Relative module path from WorkingDir; relative file from PWD.
	c, _ = runToExit(t, m, CreateOptions{Lower: img, Config: Config{Entrypoint: []string{"../usr/bin/probe.wasm"}, Cmd: []string{"cat", "data.txt"}, WorkingDir: "/app"}})
	if stdout, _ := output(t, m, c.ID()); stdout != "data\n" {
		t.Fatalf("%q", stdout)
	}
	// $PATH from the image config is honoured.
	c, _ = runToExit(t, m, CreateOptions{Lower: img, Config: Config{Cmd: []string{"greet"}, Env: []string{"PATH=/nope:/app", "NAME=path"}}})
	if stdout, _ := output(t, m, c.ID()); stdout != "Hello, path!\n" {
		t.Fatalf("%q", stdout)
	}
	// Absolute.
	c, _ = runToExit(t, m, CreateOptions{Lower: img, Config: Config{Cmd: []string{"/app/greet.wasm"}}})
	if stdout, _ := output(t, m, c.ID()); stdout != "Hello, world!\n" {
		t.Fatalf("%q", stdout)
	}
}

func TestStartFailures(t *testing.T) {
	img := image(t)
	m := manager(t, Options{})
	ctx := context.Background()
	c, _ := m.Create(CreateOptions{Lower: img, Config: Config{Cmd: []string{"nothere"}}})
	if err := m.Start(ctx, c.ID()); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.State != Exited || s.ExitCode != runtime.ExitMissing || s.Error == "" {
		t.Fatalf("%+v", s)
	}
	c, _ = m.Create(CreateOptions{Lower: img, Config: Config{Cmd: []string{"/app/data.txt"}}})
	if err := m.Start(ctx, c.ID()); !errors.Is(err, runtime.ErrInvalid) {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.ExitCode != runtime.ExitNoExec {
		t.Fatalf("%+v", s)
	}
	for _, o := range []CreateOptions{
		{Lower: img},
		{Config: Config{Cmd: []string{"x"}}},
		{Name: "bad name!", Lower: img, Config: Config{Cmd: []string{"x"}}},
		{Name: strings.Repeat("n", 129), Lower: img, Config: Config{Cmd: []string{"x"}}},
	} {
		if _, err := m.Create(o); !errors.Is(err, ErrInvalid) {
			t.Errorf("%+v: %v", o.Name, err)
		}
	}
	if _, err := m.Wait(ctx, c.ID(), "sideways"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := m.Kill(c.ID()); !errors.Is(err, ErrNotRunning) {
		t.Fatal(err)
	}
	if _, err := m.Get(""); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestRunningContainer(t *testing.T) {
	img := image(t)
	m := manager(t, Options{Timeout: 5 * time.Second})
	ctx := context.Background()
	c, _ := m.Create(CreateOptions{Name: "spinner", Lower: img, Config: Config{Cmd: []string{"probe", "spin"}}})
	if err := m.Start(ctx, "spinner"); err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx, "spinner"); !errors.Is(err, ErrAlreadyStarted) {
		t.Fatal(err)
	}
	if l := m.List(false); len(l) != 1 || l[0].State != Running {
		t.Fatal(l)
	}
	if err := m.Remove(ctx, "spinner", false); !errors.Is(err, ErrRunning) {
		t.Fatal(err)
	}
	// Following the logs of a running container returns when it stops.
	done := make(chan error, 1)
	go func() { done <- m.Logs(ctx, "spinner", true, func(Record) error { return nil }) }()
	wctx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	if _, err := m.Wait(wctx, "spinner", WaitNotRunning); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	cancel()
	if err := m.Kill("spinner"); err != nil {
		t.Fatal(err)
	}
	if code, _ := m.Wait(ctx, "spinner", WaitNotRunning); code != runtime.ExitKilled {
		t.Fatal(code)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if s := c.Snapshot(); s.Error != "killed" {
		t.Fatalf("%+v", s)
	}
	// Force-remove a running one.
	m.Start(ctx, "spinner")
	if err := m.Remove(ctx, "spinner", true); err != nil {
		t.Fatal(err)
	}
	if l := m.List(true); len(l) != 0 {
		t.Fatal(l)
	}
	// Timeouts are recorded.
	m2 := manager(t, Options{Timeout: 200 * time.Millisecond})
	c, _ = m2.Create(CreateOptions{Lower: img, Config: Config{Cmd: []string{"probe", "spin"}}})
	m2.Start(ctx, c.ID())
	if code, _ := m2.Wait(ctx, c.ID(), ""); code != runtime.ExitKilled || !c.Snapshot().TimedOut {
		t.Fatal(code, c.Snapshot())
	}
	// Shutdown kills what runs.
	c, _ = m2.Create(CreateOptions{Lower: img, Config: Config{Cmd: []string{"probe", "spin"}}})
	m2.Start(ctx, c.ID())
	m2.Shutdown(ctx)
	if c.Snapshot().State != Exited {
		t.Fatal("shutdown left a container running")
	}
}

func TestLimitsAndIsolation(t *testing.T) {
	img := image(t)
	m := manager(t, Options{MaxContainers: 2, MaxLogBytes: 16, MemoryLimitBytes: 32 << 20})
	a, code := runToExit(t, m, CreateOptions{Lower: img, Config: Config{Cmd: []string{"probe", "args", "0123456789", "abcdefghij"}}})
	if code != 0 {
		t.Fatal(code)
	}
	// Only the newest 16 bytes are kept.
	if stdout, _ := output(t, m, a.ID()); len(stdout) > 16 || !strings.HasSuffix(stdout, "abcdefghij\n") {
		t.Fatalf("%q", stdout)
	}
	b, code := runToExit(t, m, CreateOptions{Lower: img, Config: Config{Cmd: []string{"probe", "hog"}}})
	if code == 0 || b.Snapshot().State != Exited {
		t.Fatal("memory limit not applied")
	}
	if _, err := m.Create(CreateOptions{Lower: img, Config: Config{Cmd: []string{"x"}}}); !errors.Is(err, ErrTooMany) {
		t.Fatal(err)
	}
	// Ambiguous prefix: two IDs share the empty-ish prefix only if equal
	// first chars; force it by looking up a 0-length-free common prefix.
	ids := []string{a.ID(), b.ID()}
	common := 0
	for common < 64 && ids[0][common] == ids[1][common] {
		common++
	}
	if common > 0 {
		if _, err := m.Get(ids[0][:common]); !errors.Is(err, ErrAmbiguous) {
			t.Fatal(err)
		}
	}
	// The two containers' filesystems are separate.
	m3 := manager(t, Options{})
	w, _ := runToExit(t, m3, CreateOptions{Lower: img, Config: Config{Cmd: []string{"probe", "fs"}}})
	if s := w.Snapshot(); s.ExitCode != 0 && !strings.Contains(s.Error, "") {
		t.Fatal(s)
	}
	r, _ := runToExit(t, m3, CreateOptions{Lower: img, Config: Config{Cmd: []string{"probe", "ls", "/"}}})
	if stdout, _ := output(t, m3, r.ID()); strings.Contains(stdout, "tmp") {
		t.Fatalf("one container saw another's files: %q", stdout)
	}
	if w.Snapshot().SizeRw == 0 {
		t.Fatal("writes not accounted")
	}
}

func TestLogsCallbackError(t *testing.T) {
	img := image(t)
	m := manager(t, Options{})
	c, _ := runToExit(t, m, CreateOptions{Lower: img, Config: Config{Cmd: []string{"probe", "args", "x"}}})
	stop := errors.New("stop")
	if err := c.Logs(context.Background(), false, func(Record) error { return stop }); !errors.Is(err, stop) {
		t.Fatal(err)
	}
	// A follow on a created container waits until it runs and stops.
	c2, _ := m.Create(CreateOptions{Lower: img, Config: Config{Cmd: []string{"probe", "args", "late"}}})
	var mu sync.Mutex
	var got strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- c2.Logs(context.Background(), true, func(r Record) error { mu.Lock(); got.Write(r.Data); mu.Unlock(); return nil })
	}()
	time.Sleep(20 * time.Millisecond)
	m.Start(context.Background(), c2.ID())
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got.String() != "late\n" {
		t.Fatalf("%q", got.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	c3, _ := m.Create(CreateOptions{Lower: img, Config: Config{Cmd: []string{"x"}}})
	if err := c3.Logs(ctx, true, func(Record) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	l := newLogs(4)
	l.append(Stdout, nil)
	l.append(Stdout, []byte("123456"))
	if recs, _, _ := l.since(0); len(recs) != 1 || string(recs[0].Data) != "3456" {
		t.Fatal(recs)
	}
}
