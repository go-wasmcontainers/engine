package wazero

import (
	"context"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
)

// TestMountsThroughWASI: a real Go guest sees a volume at its path (Go's
// wasip1 port picks the longest matching preopen), writes into the volume
// and not the container, cannot write a read-only one, and gets EXDEV
// renaming across, which os.Rename reports.
func TestMountsThroughWASI(t *testing.T) {
	img := lower(t, "probe")
	e := New(opts())
	defer e.Close(context.Background())
	vol := rootfs.New()
	seed := rootfs.New()
	seed.WriteFile("/seed", []byte("seeded"), 0o644)
	spec := func(args ...string) runtime.Spec {
		root := img.Clone(rootfs.Quota{})
		root.MkdirAll("/data", 0o755) // the mountpoints, as the container manager makes them
		root.MkdirAll("/ro", 0o755)
		return runtime.Spec{FS: root, Path: "/bin/probe.wasm", Args: append([]string{"probe"}, args...),
			Mounts: []runtime.Mount{{Path: "/data", FS: vol}, {Path: "/ro", FS: seed, ReadOnly: true}}}
	}
	if r := start(t, e, spec("write", "/data/hello", "hi")); r.res.ExitCode != 0 {
		t.Fatalf("%+v", r)
	}
	if b, err := vol.ReadFile("/hello"); err != nil || string(b) != "hi" {
		t.Fatalf("volume: %q %v", b, err)
	}
	// Another container, the same volume: it is there.
	if r := start(t, e, spec("cat", "/data/hello")); r.res.ExitCode != 0 || r.stdout != "hi" {
		t.Fatalf("%+v", r)
	}
	// Relative to the working directory, too.
	sp := spec("cat", "hello")
	sp.Env = []string{"PWD=/data"}
	if r := start(t, e, sp); r.res.ExitCode != 0 || r.stdout != "hi" {
		t.Fatalf("relative: %+v", r)
	}
	if r := start(t, e, spec("cat", "/ro/seed")); r.stdout != "seeded" {
		t.Fatalf("%+v", r)
	}
	if r := start(t, e, spec("write", "/ro/x", "no")); r.res.ExitCode != 1 || !strings.Contains(strings.ToLower(r.stderr), "read-only file system") {
		t.Fatalf("read-only write: %+v", r)
	}
	// Across trees the rename fails and nothing moves: the engine's WASI
	// answers EXDEV, wazero's ENOSYS (its path_rename does not handle
	// renames across filesystems).
	want := "not implemented"
	if engineWASI {
		want = "cross-device"
	}
	if r := start(t, e, spec("mv", "/etc/motd", "/data/motd")); r.res.ExitCode != 1 || !strings.Contains(strings.ToLower(r.stderr), want) {
		t.Fatalf("rename across, want %q: %+v", want, r)
	}
	if _, err := vol.Stat("/motd"); err == nil {
		t.Fatal("a rename across mounts moved the file")
	}
	if r := start(t, e, spec("ls", "/data")); r.stdout != "hello\n" {
		t.Fatalf("ls: %+v", r)
	}
}
