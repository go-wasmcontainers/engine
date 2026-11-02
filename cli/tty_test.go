package cli

import (
	"bytes"
	"context"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/internal/testutil"
)

// lockedBuffer is a bytes.Buffer safe for the command's writer and the
// test's reader.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// gatedInput yields first, then waits (up to 30 s) until the command's
// output holds marker before yielding rest.
type gatedInput struct {
	out          *lockedBuffer
	first, rest  string
	marker       string
	sentFirst    bool
	waited, done bool
}

func (g *gatedInput) Read(p []byte) (int, error) {
	if !g.sentFirst {
		g.sentFirst = true
		return copy(p, g.first), nil
	}
	if !g.waited {
		g.waited = true
		for end := time.Now().Add(30 * time.Second); time.Now().Before(end) && !strings.Contains(g.out.String(), g.marker); {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if g.done {
		return 0, io.EOF
	}
	g.done = true
	return copy(p, g.rest), nil
}

// wctrGated is wctrIn with the input sent in two parts: rest goes only
// once the output holds marker.
func wctrGated(t *testing.T, host, first, marker, rest string, args ...string) result {
	t.Helper()
	cmd := RootCmd(Options{})
	out, errb := &lockedBuffer{}, &lockedBuffer{}
	cmd.SetOut(out)
	cmd.SetErr(errb)
	cmd.SetIn(&gatedInput{out: out, first: first, rest: rest, marker: marker})
	cmd.SetArgs(append([]string{"-H", host}, args...))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	code := 0
	if err := cmd.ExecuteContext(ctx); err != nil {
		code = Report(errb, err)
	}
	return result{out.String(), errb.String(), code}
}

// TestRunInteractive: run -i sends this client's input to the container's
// first process, run -it gives it a terminal (the engine's line
// discipline: echo, CR LF, ^D), the exit status comes back, the detach
// keys leave the container running, attach and start -ai reattach, and
// resize reaches the terminal.
func TestRunInteractive(t *testing.T) {
	dir := shortDir(t)
	layout := filepath.Join(dir, "layout")
	testutil.WriteLayout(t, layout, testutil.BuildContent(testutil.ImageSpec{
		RefName: "probe", Module: testutil.Wasm(t, "probe"), ModulePath: "/bin/probe.wasm",
		Files: map[string]string{"bin/wasixguest.wasm": string(testutil.Wasm(t, "wasixguest"))},
		Cmd:   []string{"spin"},
	}))
	h := execDaemon(t, dir)
	must(t, wctr(t, h, "pull", "oci:"+layout+":probe"), 0)

	if r := must(t, wctrIn(t, h, "piped in\n", "run", "--rm", "-i", "--entrypoint", "wasixguest", "probe", "child-cat"), 0); r.stdout != "piped in\n" {
		t.Errorf("run -i: %+v", r)
	}
	if r := must(t, wctrIn(t, h, "hi\r\x04", "run", "--rm", "-it", "--entrypoint", "wasixguest", "probe", "child-cat"), 0); r.stdout != "hi\r\nhi\r\n" {
		t.Errorf("run -it: %q", r.stdout)
	}
	if r := wctrIn(t, h, "", "run", "--rm", "-i", "probe", "exit", "4"); r.code != 4 {
		t.Errorf("run -i exit status: %+v", r)
	}
	// Without -i the input is not the container's.
	if r := must(t, wctrIn(t, h, "ignored\n", "run", "--rm", "--entrypoint", "wasixguest", "probe", "child-cat"), 0); r.stdout != "" {
		t.Errorf("run without -i read stdin: %q", r.stdout)
	}

	// Detach and reattach.
	must(t, wctr(t, h, "run", "-dit", "--name", "d", "--entrypoint", "wasixguest", "probe", "child-cat"), 0)
	// The detach keys must follow the echo of "one": the server ends the
	// output stream at once on the detach keys, dropping whatever it has
	// not written yet (engine#56). So the keys wait for the echo.
	r := wctrGated(t, h, "one\r", "one\r\n", "\x10\x11ignored\r", "attach", "d")
	if r.code != 0 || !strings.Contains(r.stdout, "one\r\n") || strings.Contains(r.stdout, "ignored") {
		t.Fatalf("attach, then ctrl-p ctrl-q: %+v", r)
	}
	c, err := api.NewClient(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if !running(ctx, c, "d") {
		t.Fatal("not running after the detach")
	}
	if err := c.ContainerResize(ctx, "d", 40, 100); err != nil {
		t.Errorf("resize: %v", err)
	}
	if err := c.ContainerResize(ctx, "d", 0, 100); err == nil {
		t.Error("resized to 0 rows")
	}
	// A custom sequence, then ^D on an empty line: end of input, the
	// container exits, attach returns its status.
	r = wctrIn(t, h, "two\r\x04", "attach", "--detach-keys", "ctrl-x", "d")
	if r.code != 0 || !strings.Contains(r.stdout, "two\r\n") {
		t.Fatalf("reattach: %+v", r)
	}
	waitExited(t, c, "d")
	if r := wctr(t, h, "attach", "d"); r.code == 0 {
		t.Errorf("attach to a stopped container: %+v", r)
	}
	if r := must(t, wctrIn(t, h, "three\r\x04", "start", "-ai", "d"), 0); !strings.Contains(r.stdout, "three\r\n") {
		t.Errorf("start -ai: %+v", r)
	}
	if err := c.ContainerResize(ctx, "d", 10, 10); err == nil {
		t.Error("resized a stopped container")
	}
	must(t, wctr(t, h, "rm", "-f", "d"), 0)

	// A container without -t has no terminal to resize; without -i,
	// attach sends nothing.
	must(t, wctr(t, h, "run", "-d", "--name", "plain", "probe"), 0)
	if err := c.ContainerResize(ctx, "plain", 10, 10); err == nil {
		t.Error("resized a container without a terminal")
	}
	must(t, wctr(t, h, "rm", "-f", "plain"), 0)
	if r := wctr(t, h, "run", "--rm", "-it", "--detach-keys", "ctrl-1", "probe"); r.code != 125 {
		t.Errorf("bad detach keys: %+v", r)
	}
}

func waitExited(t *testing.T, c *api.Client, id string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for running(context.Background(), c, id) {
		if time.Now().After(deadline) {
			t.Fatalf("%s still running", id)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
