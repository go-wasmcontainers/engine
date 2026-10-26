package wasi

import (
	"encoding/binary"
	"testing"

	"github.com/go-wasmcontainers/engine/rootfs"
)

func TestMounts(t *testing.T) {
	root := rootfs.New()
	root.MkdirAll("/etc", 0o755)
	root.WriteFile("/etc/motd", []byte("hello"), 0o644)
	data, ro := rootfs.New(), rootfs.New()
	ro.WriteFile("/seed", []byte("s"), 0o644)
	p := New(Config{FS: root, Mounts: []Mount{{Path: "/data", FS: data}, {Path: "/ro", FS: ro, ReadOnly: true}}})
	m := make(mem, 4096)

	// The preopens, in order: "/", "/data", "/ro"; then nothing.
	for fd, name := range map[uint64]string{3: "/", 4: "/data", 5: "/ro"} {
		if r := call(t, p, m, "fd_prestat_get", fd, 0); r.Errno != 0 || binary.LittleEndian.Uint32(m[4:]) != uint32(len(name)) {
			t.Fatalf("fd %d: %v %d", fd, r, binary.LittleEndian.Uint32(m[4:]))
		}
		if r := call(t, p, m, "fd_prestat_dir_name", fd, 100, uint64(len(name))); r.Errno != 0 || string(m[100:100+len(name)]) != name {
			t.Fatalf("fd %d: %v %q", fd, r, m[100:100+len(name)])
		}
	}
	if r := call(t, p, m, "fd_prestat_get", 6, 0); r.Errno != EBADF {
		t.Fatal("a fourth preopen")
	}
	if r := call(t, p, m, "fd_prestat_dir_name", 4, 100, 2); r.Errno != EINVAL {
		t.Fatal("a short buffer accepted")
	}

	open := func(dirfd uint64, path string, oflags, rights uint64) (uint32, uint32) {
		a, n := put(m, 200, path)
		r := call(t, p, m, "path_open", dirfd, 0, a, n, oflags, rights, rights, 0, 300)
		return r.Errno, binary.LittleEndian.Uint32(m[300:])
	}
	// Writing through /data lands in the volume, not the root.
	if e, fd := open(4, "x", oflagCreat, rightFdWrite|rightFdRead); e != 0 {
		t.Fatal(e)
	} else {
		a, _ := put(m, 400, "vol")
		binary.LittleEndian.PutUint32(m[500:], uint32(a))
		binary.LittleEndian.PutUint32(m[504:], 3)
		if r := call(t, p, m, "fd_write", uint64(fd), 500, 1, 600); r.Errno != 0 {
			t.Fatal(r)
		}
	}
	if b, _ := data.ReadFile("/x"); string(b) != "vol" {
		t.Fatalf("volume holds %q", b)
	}
	if _, err := root.Stat("/x"); err == nil {
		t.Fatal("the write reached the root")
	}
	// The read-only mount reads, and refuses every change.
	if e, _ := open(5, "seed", 0, rightFdRead); e != 0 {
		t.Fatal(e)
	}
	if e, _ := open(5, "seed", 0, rightFdWrite); e != EROFS {
		t.Fatalf("write open: %d", e)
	}
	if e, _ := open(5, "new", oflagCreat, rightFdRead); e != EROFS {
		t.Fatalf("create: %d", e)
	}
	a, n := put(m, 200, "d")
	if r := call(t, p, m, "path_create_directory", 5, a, n); r.Errno != EROFS {
		t.Fatal(r)
	}
	a, n = put(m, 200, "seed")
	if r := call(t, p, m, "path_unlink_file", 5, a, n); r.Errno != EROFS {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_filestat_get", 5, 700); r.Errno != 0 || m[716] != ftDirectory {
		t.Fatal(r)
	}
	// Across trees: EXDEV, as across mounts on Linux.
	oa, on := put(m, 200, "etc/motd")
	na, nn := put(m, 260, "motd")
	if r := call(t, p, m, "path_rename", 3, oa, on, 4, na, nn); r.Errno != EXDEV {
		t.Fatalf("rename across: %v", r)
	}
	if r := call(t, p, m, "path_link", 3, 0, oa, on, 4, na, nn); r.Errno != EXDEV {
		t.Fatalf("link across: %v", r)
	}
	// Within one tree, rename works.
	oa, on = put(m, 200, "x")
	na, nn = put(m, 260, "y")
	if r := call(t, p, m, "path_rename", 4, oa, on, 4, na, nn); r.Errno != 0 {
		t.Fatal(r)
	}
	if _, err := data.Stat("/y"); err != nil {
		t.Fatal(err)
	}
}
