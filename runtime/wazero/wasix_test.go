package wazero

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
)

// The WASIX host module (wasi.WASIX), driven by testdata/wasm/wasixguest,
// whose bindings are written from wasix-libc's api_wasix.h. Four of these
// cases are defects of wasmer 7.5.0, the independent WASIX host the shell's
// guest side is checked against; the engine must not share them.

func wasixExecutor(t *testing.T) *Executor {
	o := opts()
	o.HostModules = []wasi.HostModule{wasi.WASIX}
	e := New(o)
	t.Cleanup(func() { e.Close(context.Background()) })
	return e
}

func wasixRun(t *testing.T, e *Executor, spec runtime.Spec, args ...string) run {
	t.Helper()
	img := lower(t, "wasixguest")
	spec.FS = img.Clone(rootfs.Quota{})
	spec.Path = "/bin/wasixguest.wasm"
	spec.Args = append([]string{"wasixguest"}, args...)
	if spec.Env == nil {
		spec.Env = []string{"PWD=/"}
	}
	done := make(chan run, 1)
	go func() { done <- start(t, e, spec) }()
	select {
	case r := <-done:
		return r
	case <-time.After(60 * time.Second):
		t.Fatalf("%v: did not end", args)
		return run{}
	}
}

func TestWASIX(t *testing.T) {
	e := wasixExecutor(t)
	cases := []struct {
		name, arg, want string
	}{
		{"proc_spawn3 keeps empty and newline arguments", "argv", `["" "a\nb" "c"]` + "\n0\n"},
		{"proc_spawn2 splits on line feeds, drops empty entries", "spawn2", `["x" "y"]` + "\n"},
		// wasmer defect 2: exit codes above 80 came back as 79.
		{"exit codes travel raw", "codes", "1:0 1:3 1:78 1:79 1:255 1:300\n"},
		// wasmer defect 1: fdstat of fds 0-2 was hard-coded, so a Go child
		// with a pipe on stdin read EAGAIN instead of polling.
		{"a Go child reads a pipe that fills late", "latestdin", "late\ncode 0\n"},
		// wasmer defect 3: no filestat on a pipe end.
		{"a pipe end has a filestat", "fstat", "fstat ok\n0\n"},
		// wasmer defect 4: poll_oneoff never returned for an empty pipe.
		{"a goroutine parked on an empty pipe wakes", "readerfirst", "lines 20000\n"},
		{"join any child, then ECHILD", "joinany", fmt.Sprintf("[1 2] then %d\n", wasi.ECHILD)},
		{"non-blocking join of a running child", "nonblock", "running 0 0\nended 1 0 0\n"},
		{"spawn errors", "errors", fmt.Sprintf("%d %d %d %d %d %d\n", wasi.ENOENT, wasi.ENOEXEC, wasi.ENOENT, wasi.EINVAL, wasi.EINVAL, wasi.EBADF)},
		{"PATH search finds <dir>/<name>.wasm", "search", `["found"]` + "\n"},
		{"CHDIR op and $PWD", "chdir", "/etc\n"},
		{"OPEN op redirects the child's stdout", "redirect", "[\"in a file\"]\n<nil>\n"},
		{"proc_id and proc_parent", "ids", "parent 1 child 2 1\n"},
		{"tty_get without a terminal", "tty", "0 0 0 0 0 0 0 0\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := wasixRun(t, e, runtime.Spec{}, c.arg)
			if r.stdout != c.want || r.res.ExitCode != 0 {
				t.Fatalf("stdout %q, want %q (exit %d %s, stderr %q)", r.stdout, c.want, r.res.ExitCode, r.res.Error, r.stderr)
			}
		})
	}
}

func TestWASIXTerminal(t *testing.T) {
	e := wasixExecutor(t)
	r := wasixRun(t, e, runtime.Spec{Terminal: wasi.NewTerminal(120, 40, [3]bool{true, true, false})}, "tty")
	if r.stdout != "0 120 40 1 1 0 1 1\n" {
		t.Fatalf("%q (stderr %q)", r.stdout, r.stderr)
	}
}

// TestWASIXForkBomb: spawning without end stops at the group's process
// limit with EAGAIN, and the container still stops (its children are
// killed with it).
func TestWASIXForkBomb(t *testing.T) {
	e := wasixExecutor(t)
	r := wasixRun(t, e, runtime.Spec{Processes: wasi.GroupLimits{MaxProcs: 8, SpawnRate: 1000, SpawnBurst: 1000}}, "bomb")
	if want := fmt.Sprintf("refused after 7 with %d\n", wasi.EAGAIN); r.stdout != want {
		t.Fatalf("%q, want %q (stderr %q)", r.stdout, want, r.stderr)
	}
	if r.res.ExitCode != 0 || r.stderr != "" {
		t.Fatalf("%+v, stderr %q", r.res, r.stderr)
	}
}
