package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/internal/testutil"
)

func shortDir(t *testing.T) string {
	d, err := os.MkdirTemp("", "wc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

func TestServeAndSocket(t *testing.T) {
	dir := shortDir(t)
	sock := filepath.Join(dir, "run", "d.sock")
	if err := os.Mkdir(filepath.Dir(sock), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := listenUnix(filepath.Join(dir, "missing", "d.sock")); err == nil {
		t.Fatal("listened in a directory that does not exist")
	}
	ready := make(chan string, 1)
	cmd := rootCmd(ready)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"-H", "unix://" + sock, "--data-root", filepath.Join(dir, "data")})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- cmd.ExecuteContext(ctx) }()
	select {
	case <-ready:
	case err := <-done:
		t.Fatal(err, out.String())
	}
	st, err := os.Stat(sock)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 || st.Mode()&os.ModeSocket == 0 {
		t.Fatalf("socket mode %v", st.Mode())
	}
	// The parent directory is left as it was, and no staging dir remains.
	if st, _ := os.Stat(filepath.Dir(sock)); st.Mode().Perm() != 0o755 {
		t.Fatalf("socket dir mode changed to %v", st.Mode())
	}
	if ents, _ := os.ReadDir(filepath.Dir(sock)); len(ents) != 1 {
		t.Fatalf("staging left behind: %v", ents)
	}
	// A second daemon on the same socket refuses.
	if _, err := listenUnix(sock); err == nil || !strings.Contains(err.Error(), "another daemon") {
		t.Fatal(err)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(sock); !os.IsNotExist(err) {
		t.Fatal("socket left behind")
	}
	if !strings.Contains(out.String(), "listening on unix://") || !strings.Contains(out.String(), "shutting down") {
		t.Fatal(out.String())
	}
}

func TestListenRefusals(t *testing.T) {
	dir := shortDir(t)
	file := filepath.Join(dir, "plain")
	os.WriteFile(file, []byte("x"), 0o644)
	if _, err := listenUnix(file); err == nil || !strings.Contains(err.Error(), "not a socket") {
		t.Fatal(err)
	}
	if _, err := listenUnix(filepath.Join(file, "x.sock")); err == nil {
		t.Fatal("socket under a file")
	}
	// A stale socket (nobody listening) is replaced.
	stale := filepath.Join(dir, "stale.sock")
	l, err := net.Listen("unix", stale)
	if err != nil {
		t.Fatal(err)
	}
	l.(*net.UnixListener).SetUnlinkOnClose(false)
	l.Close()
	l2, err := listenUnix(stale)
	if err != nil {
		t.Fatal(err)
	}
	l2.Close()

	for _, args := range [][]string{
		{"-H", "tcp://0.0.0.0:2375"},
		{"--memory", "lots"},
		{"--storage-quota", "lots"},
		{"--data-root", file, "-H", "unix://" + filepath.Join(dir, "s.sock")},
	} {
		cmd := RootCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
	code := -1
	osExit = func(c int) { code = c }
	defer func() { osExit = os.Exit }()
	os.Args = []string{"wctrd", "-H", "tcp://x"}
	main()
	if code != 1 {
		t.Fatal(code)
	}
	if defaultDataRoot() == "" {
		t.Fatal("no default data root")
	}
	t.Setenv("XDG_DATA_HOME", "/xdg")
	if defaultDataRoot() != "/xdg/wasmcontainers" {
		t.Fatal(defaultDataRoot())
	}
}

// TestAcceptance is the README demo, end to end, with the real binaries:
// wctrd in the background, then wctr pull, run, ps, logs, rm.
func TestAcceptance(t *testing.T) {
	if testing.Short() {
		t.Skip("builds and runs the binaries")
	}
	dir := shortDir(t)
	bin := filepath.Join(dir, "bin")
	for _, c := range []string{"wctr", "wctrd"} {
		b := exec.Command("go", "build", "-o", filepath.Join(bin, c), "./cmd/"+c)
		b.Dir = testutil.RepoRoot()
		b.Env = append(os.Environ(), "CGO_ENABLED=0", "GOWORK=off")
		if msg, err := b.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", c, err, msg)
		}
	}
	layout := filepath.Join(dir, "layout")
	testutil.WriteLayout(t, layout, testutil.BuildContent(
		testutil.ImageSpec{RefName: "hello", Module: testutil.Wasm(t, "hello"), ModulePath: "/bin/hello.wasm"},
		testutil.ImageSpec{RefName: "greet", Module: testutil.Wasm(t, "greet"), Artifact: true},
		testutil.ImageSpec{RefName: "probe", Module: testutil.Wasm(t, "probe"), ModulePath: "/bin/probe.wasm"},
	))
	sock := filepath.Join(dir, "d.sock")
	var dlog bytes.Buffer
	d := exec.Command(filepath.Join(bin, "wctrd"), "-H", "unix://"+sock, "--data-root", filepath.Join(dir, "data"))
	d.Stdout, d.Stderr, d.Stdin = &dlog, &dlog, nil
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Process.Signal(syscall.SIGTERM) // by PID, never by name
		d.Wait()
	})
	for i := 0; ; i++ {
		if c, err := net.Dial("unix", sock); err == nil {
			c.Close()
			break
		}
		if i > 200 {
			t.Fatalf("wctrd did not come up: %s", dlog.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	var transcript strings.Builder
	sh := func(want int, args ...string) string {
		t.Helper()
		c := exec.Command(filepath.Join(bin, "wctr"), args...)
		c.Env = append(os.Environ(), "WCTR_HOST=unix://"+sock)
		var out, errb bytes.Buffer
		c.Stdout, c.Stderr, c.Stdin = &out, &errb, nil
		err := c.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		shown := strings.ReplaceAll(strings.Join(args, " "), layout, "./layout")
		transcript.WriteString("$ wctr " + shown + "\n" + out.String() + errb.String())
		if code != want {
			t.Fatalf("wctr %s: exit %d, want %d\n%s%s", shown, code, want, out.String(), errb.String())
		}
		return out.String() + errb.String()
	}
	sh(0, "pull", "oci:"+layout+":hello")
	sh(0, "pull", "oci:"+layout+":greet")
	sh(0, "pull", "oci:"+layout+":probe")
	if out := sh(0, "run", "--rm", "hello"); out != "Hello from a WebAssembly container!\n" {
		t.Fatalf("%q", out)
	}
	if out := sh(0, "run", "--rm", "-e", "NAME=x", "greet"); out != "Hello, x!\n" {
		t.Fatalf("%q", out)
	}
	sh(42, "run", "--name", "failing", "probe", "exit", "42")
	if out := sh(0, "ps", "-a"); !strings.Contains(out, "Exited (42)") {
		t.Fatal(out)
	}
	if out := sh(0, "logs", "failing"); out != "bye\n" {
		t.Fatalf("%q", out)
	}
	sh(0, "rm", "failing")
	sh(0, "ps", "-a")
	sh(0, "images")
	t.Logf("transcript:\n%s", transcript.String())
}
