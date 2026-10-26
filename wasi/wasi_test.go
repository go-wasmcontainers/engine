package wasi

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/rootfs"
)

// mem is a flat test memory.
type mem []byte

func (m mem) Read(off, n uint32) ([]byte, bool) {
	if uint64(off)+uint64(n) > uint64(len(m)) {
		return nil, false
	}
	return m[off : off+n], true
}

func (m mem) Write(off uint32, b []byte) bool {
	if uint64(off)+uint64(len(b)) > uint64(len(m)) {
		return false
	}
	copy(m[off:], b)
	return true
}

func call(t *testing.T, p *Proc, m mem, name string, args ...uint64) Result {
	t.Helper()
	i, ok := FuncIndex[name]
	if !ok {
		t.Fatalf("no %s", name)
	}
	return p.Call(i, args, m)
}

func put(m mem, off uint32, s string) (uint64, uint64) {
	copy(m[off:], s)
	return uint64(off), uint64(len(s))
}

func newProc(t *testing.T) (*Proc, *rootfs.FS, *bytes.Buffer) {
	f := rootfs.New()
	f.MkdirAll("/etc", 0o755)
	f.WriteFile("/etc/motd", []byte("hello"), 0o644)
	var out bytes.Buffer
	return New(Config{Args: []string{"a", "bc"}, Env: []string{"K=v"}, FS: f, Stdout: &out, Stderr: &out}), f, &out
}

func TestArgsEnvClockRandom(t *testing.T) {
	p, _, _ := newProc(t)
	m := make(mem, 1024)
	if r := call(t, p, m, "args_sizes_get", 0, 4); r.Errno != 0 || binary.LittleEndian.Uint32(m[0:]) != 2 || binary.LittleEndian.Uint32(m[4:]) != 5 {
		t.Fatal(r, m[:8])
	}
	if r := call(t, p, m, "args_get", 100, 200); r.Errno != 0 || string(m[200:205]) != "a\x00bc\x00" {
		t.Fatal(r)
	}
	if binary.LittleEndian.Uint32(m[104:]) != 202 {
		t.Fatal("argv[1] pointer")
	}
	if r := call(t, p, m, "environ_sizes_get", 0, 4); r.Errno != 0 || binary.LittleEndian.Uint32(m[0:]) != 1 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "environ_get", 100, 200); r.Errno != 0 || string(m[200:204]) != "K=v\x00" {
		t.Fatal(r)
	}
	for id := uint64(0); id < 4; id++ {
		if r := call(t, p, m, "clock_time_get", id, 0, 0); r.Errno != 0 {
			t.Fatal(id, r)
		}
	}
	if r := call(t, p, m, "clock_time_get", 9, 0, 0); r.Errno != EINVAL {
		t.Fatal(r)
	}
	if r := call(t, p, m, "clock_res_get", 0, 0); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "random_get", 0, 32); r.Errno != 0 {
		t.Fatal(r)
	}
	// Out of bounds everywhere is EFAULT, never a panic.
	for _, c := range []struct {
		n    string
		args []uint64
	}{
		{"args_get", []uint64{2000, 0}}, {"args_sizes_get", []uint64{2000, 0}}, {"environ_get", []uint64{0, 2000}},
		{"clock_time_get", []uint64{0, 0, 2000}}, {"clock_res_get", []uint64{0, 2000}}, {"random_get", []uint64{1000, 100}}, {"random_get", []uint64{0, 0xffffffff}}, {"random_get", []uint64{0xffffff00, 0x1000}},
		{"fd_fdstat_get", []uint64{1, 2000}}, {"fd_filestat_get", []uint64{1, 2000}}, {"fd_prestat_get", []uint64{3, 2000}},
		{"fd_prestat_dir_name", []uint64{3, 2000, 1}}, {"fd_write", []uint64{1, 2000, 1, 0}}, {"poll_oneoff", []uint64{2000, 0, 1, 0}},
	} {
		if r := call(t, p, m, c.n, c.args...); r.Errno != EFAULT {
			t.Errorf("%s: %v", c.n, r)
		}
	}
	if r := p.Call(-1, nil, m); r.Errno != ENOSYS {
		t.Fatal(r)
	}
	if r := p.Call(0, []uint64{1}, m); r.Errno != ENOSYS {
		t.Fatal("wrong arity accepted")
	}
	if r := call(t, p, m, "proc_exit", 7); !r.Exit || r.Code != 7 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "proc_raise", 0); r.Errno != ENOSYS {
		t.Fatal(r)
	}
	// Socket calls on a descriptor that is not a socket; and no network.
	for _, n := range []string{"sock_recv", "sock_send", "sock_accept", "sock_shutdown", "sock_bind", "sock_listen",
		"sock_connect", "sock_recv_from", "sock_send_to", "sock_getsockopt", "sock_setsockopt", "sock_getlocaladdr", "sock_getpeeraddr"} {
		args := make([]uint64, len(Funcs[FuncIndex[n]].Params))
		args[0] = 1 // stdout
		if r := call(t, p, m, n, args...); r.Errno != ENOTSOCK && r.Errno != EFAULT && r.Errno != EINVAL {
			t.Error(n, r)
		}
	}
	if r := call(t, p, m, "sock_open", afInet4, sockStream, 0); r.Errno != ENOTSUP {
		t.Fatal("sock_open with no network:", r)
	}
	if r := call(t, p, m, "sched_yield"); r.Errno != 0 {
		t.Fatal(r)
	}
}

func TestFiles(t *testing.T) {
	p, f, out := newProc(t)
	m := make(mem, 4096)
	pp, pl := put(m, 1000, "etc/motd")
	// path_open(3, follow, path, oflags=0, rights read, inh, fdflags, &fd)
	if r := call(t, p, m, "path_open", 3, 1, pp, pl, 0, rightFdRead, 0, 0, 0); r.Errno != 0 {
		t.Fatal(r)
	}
	fd := uint64(binary.LittleEndian.Uint32(m[0:]))
	// iovec at 100: {ptr 200, len 3}, {ptr 300, len 10}
	binary.LittleEndian.PutUint32(m[100:], 200)
	binary.LittleEndian.PutUint32(m[104:], 3)
	binary.LittleEndian.PutUint32(m[108:], 300)
	binary.LittleEndian.PutUint32(m[112:], 10)
	if r := call(t, p, m, "fd_read", fd, 100, 2, 8); r.Errno != 0 || binary.LittleEndian.Uint32(m[8:]) != 5 || string(m[200:203]) != "hel" || string(m[300:302]) != "lo" {
		t.Fatal(r, binary.LittleEndian.Uint32(m[8:]))
	}
	if r := call(t, p, m, "fd_write", fd, 100, 1, 8); r.Errno != EBADF {
		t.Fatal("write on a read-only fd", r)
	}
	if r := call(t, p, m, "fd_pread", fd, 100, 1, 1, 8); r.Errno != 0 || string(m[200:203]) != "ell" {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_pread", fd, 100, 1, 1<<63, 8); r.Errno != EINVAL {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_seek", fd, 0, 9, 8); r.Errno != EINVAL {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_seek", fd, ^uint64(0), whenceEnd, 8); r.Errno != 0 || binary.LittleEndian.Uint64(m[8:]) != 4 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_tell", fd, 8); r.Errno != 0 || binary.LittleEndian.Uint64(m[8:]) != 4 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_filestat_get", fd, 400); r.Errno != 0 || m[416] != ftRegular || binary.LittleEndian.Uint64(m[432:]) != 5 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_fdstat_get", fd, 400); r.Errno != 0 || m[400] != ftRegular {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_readdir", fd, 0, 100, 0, 8); r.Errno != ENOTDIR {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_close", fd); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_close", fd); r.Errno != EBADF {
		t.Fatal(r)
	}
	// Create, write, append, truncate, times.
	pp, pl = put(m, 1000, "new.txt")
	if r := call(t, p, m, "path_open", 3, 1, pp, pl, oflagCreat|oflagExcl, rightFdRead|rightFdWrite, 0, fdflagAppend, 0); r.Errno != 0 {
		t.Fatal(r)
	}
	fd = uint64(binary.LittleEndian.Uint32(m[0:]))
	copy(m[200:], "abc")
	if r := call(t, p, m, "fd_write", fd, 100, 1, 8); r.Errno != 0 || binary.LittleEndian.Uint32(m[8:]) != 3 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_open", 3, 1, pp, pl, oflagCreat|oflagExcl, 0, 0, 0, 0); r.Errno != EEXIST {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_pwrite", fd, 100, 1, 1, 8); r.Errno != 0 {
		t.Fatal(r)
	}
	if b, _ := f.ReadFile("/new.txt"); string(b) != "aabc" {
		t.Fatalf("%q", b)
	}
	if r := call(t, p, m, "fd_fdstat_set_flags", fd, 0); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_filestat_set_size", fd, 2); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_filestat_set_size", fd, 1<<63); r.Errno != EINVAL {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_filestat_set_times", fd, 0, uint64(time.Unix(5, 0).UnixNano()), fstMtim); r.Errno != 0 {
		t.Fatal(r)
	}
	if st, _ := f.Stat("/new.txt"); st.Size() != 2 || st.ModTime().Unix() != 5 {
		t.Fatal(st.Size(), st.ModTime())
	}
	for _, n := range []string{"fd_sync", "fd_datasync"} {
		if r := call(t, p, m, n, fd); r.Errno != 0 {
			t.Fatal(n, r)
		}
	}
	if r := call(t, p, m, "fd_fdstat_set_rights", fd, 0, 0); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_advise", fd, 0, 0, 0); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_allocate", 99, 0, 0); r.Errno != EBADF {
		t.Fatal(r)
	}
	// renumber fd onto 5 after opening something at 5.
	pp2, pl2 := put(m, 1100, "etc")
	call(t, p, m, "path_open", 3, 1, pp2, pl2, oflagDirectory, rightFdRead, 0, 0, 0)
	dfd := uint64(binary.LittleEndian.Uint32(m[0:]))
	if r := call(t, p, m, "fd_renumber", fd, dfd); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_renumber", fd, dfd); r.Errno != EBADF {
		t.Fatal(r)
	}
	// stdio.
	copy(m[200:], "out")
	if r := call(t, p, m, "fd_write", 1, 100, 1, 8); r.Errno != 0 || out.String() != "out" {
		t.Fatal(r, out.String())
	}
	if r := call(t, p, m, "fd_write", 2, 100, 1, 8); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_read", 0, 100, 1, 8); r.Errno != 0 || binary.LittleEndian.Uint32(m[8:]) != 0 {
		t.Fatal("stdin is EOF", r)
	}
	if r := call(t, p, m, "fd_seek", 1, 0, 0, 8); r.Errno != ESPIPE {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_pread", 0, 100, 1, 0, 8); r.Errno != ESPIPE {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_filestat_get", 1, 400); r.Errno != 0 || m[416] != ftCharDev {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_fdstat_get", 0, 400); r.Errno != 0 || m[400] != ftCharDev {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_write", 3, 100, 1, 8); r.Errno != EISDIR {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_write", 1, 100, 5000, 8); r.Errno != EINVAL {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_read", 1, 100, 1, 8); r.Errno != EBADF {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_filestat_set_size", 3, 0); r.Errno != EISDIR {
		t.Fatal(r)
	}
}

func TestPaths(t *testing.T) {
	p, f, _ := newProc(t)
	m := make(mem, 4096)
	path := func(s string) (uint64, uint64) { return put(m, 1000, s) }
	a, n := path("d")
	if r := call(t, p, m, "path_create_directory", 3, a, n); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_create_directory", 3, a, n); r.Errno != EEXIST {
		t.Fatal(r)
	}
	a, n = path("/etc")
	if r := call(t, p, m, "path_filestat_get", 3, 1, a, n, 400); r.Errno != ENOTCAPABLE {
		t.Fatal("absolute path accepted", r)
	}
	a, n = path("")
	if r := call(t, p, m, "path_filestat_get", 3, 1, a, n, 400); r.Errno != ENOENT {
		t.Fatal(r)
	}
	a, n = path("e\x00x")
	if r := call(t, p, m, "path_filestat_get", 3, 1, a, n, 400); r.Errno != EINVAL {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_filestat_get", 3, 1, 0, 5000, 400); r.Errno != ENAMETOOLONG {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_filestat_get", 3, 1, 4000, 200, 400); r.Errno != EFAULT {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_filestat_get", 99, 1, 0, 1, 400); r.Errno != EBADF {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_filestat_get", 1, 1, 0, 1, 400); r.Errno != ENOTDIR {
		t.Fatal(r)
	}
	// symlink, readlink, lstat vs stat, link, rename, unlink, rmdir.
	copy(m[2000:], "etc/motd")
	a, n = path("l")
	if r := call(t, p, m, "path_symlink", 2000, 8, 3, a, n); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_symlink", 4090, 100, 3, a, n); r.Errno != EFAULT {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_readlink", 3, a, n, 200, 4, 8); r.Errno != 0 || string(m[200:204]) != "etc/" || binary.LittleEndian.Uint32(m[8:]) != 4 {
		t.Fatal("readlink truncates to the buffer", r)
	}
	if r := call(t, p, m, "path_filestat_get", 3, 0, a, n, 400); r.Errno != 0 || m[416] != ftSymlink {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_filestat_get", 3, 1, a, n, 400); r.Errno != 0 || m[416] != ftRegular {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_open", 3, 0, a, n, 0, rightFdRead, 0, 0, 0); r.Errno != ELOOP {
		t.Fatal("O_NOFOLLOW on a symlink", r)
	}
	if r := call(t, p, m, "path_filestat_set_times", 3, 1, a, n, 0, 0, fstMtimNow); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_filestat_set_times", 3, 0, a, n, 0, 0, fstAtim); r.Errno != 0 {
		t.Fatal(r)
	}
	copy(m[2000:], "etc/motd")
	copy(m[2100:], "hard")
	if r := call(t, p, m, "path_link", 3, 0, 2000, 8, 3, 2100, 4); r.Errno != 0 {
		t.Fatal(r)
	}
	copy(m[2200:], "moved")
	if r := call(t, p, m, "path_rename", 3, 2100, 4, 3, 2200, 5); r.Errno != 0 {
		t.Fatal(r)
	}
	if b, _ := f.ReadFile("/moved"); string(b) != "hello" {
		t.Fatal(string(b))
	}
	a, n = path("moved")
	if r := call(t, p, m, "path_unlink_file", 3, a, n); r.Errno != 0 {
		t.Fatal(r)
	}
	a, n = path("d")
	if r := call(t, p, m, "path_unlink_file", 3, a, n); r.Errno != EISDIR {
		t.Fatal(r)
	}
	// Relative to a directory fd.
	if r := call(t, p, m, "path_open", 3, 1, a, n, oflagDirectory, rightFdRead, 0, 0, 0); r.Errno != 0 {
		t.Fatal(r)
	}
	dfd := uint64(binary.LittleEndian.Uint32(m[0:]))
	a, n = path("inner")
	if r := call(t, p, m, "path_create_directory", dfd, a, n); r.Errno != 0 {
		t.Fatal(r)
	}
	if _, err := f.Stat("/d/inner"); err != nil {
		t.Fatal(err)
	}
	if r := call(t, p, m, "path_remove_directory", dfd, a, n); r.Errno != 0 {
		t.Fatal(r)
	}
	a, n = path("d")
	if r := call(t, p, m, "path_remove_directory", 3, a, n); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "path_open", 3, 1, 0, 0, 0, 0, 0, 0, 0); r.Errno != ENOENT {
		t.Fatal(r)
	}
	// Read-only tree.
	f.Freeze()
	a, n = path("x")
	if r := call(t, p, m, "path_create_directory", 3, a, n); r.Errno != EROFS {
		t.Fatal(r)
	}
	// Too many descriptors.
	q, _, _ := newProc(t)
	a, n = path("etc")
	var last Result
	for i := 0; i < MaxFDs+2; i++ {
		last = call(t, q, m, "path_open", 3, 1, a, n, 0, rightFdRead, 0, 0, 0)
	}
	if last.Errno != 33 {
		t.Fatal("EMFILE expected", last)
	}
}

func TestReaddirAndPrestat(t *testing.T) {
	p, f, _ := newProc(t)
	for _, n := range []string{"a", "bb", "ccc"} {
		f.WriteFile("/etc/"+n, nil, 0o644)
	}
	m := make(mem, 4096)
	if r := call(t, p, m, "fd_prestat_get", 3, 0); r.Errno != 0 || binary.LittleEndian.Uint32(m[4:]) != 1 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_prestat_get", 4, 0); r.Errno != EBADF {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_prestat_dir_name", 3, 0, 1); r.Errno != 0 || m[0] != '/' {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_prestat_dir_name", 3, 0, 0); r.Errno != EINVAL {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_prestat_dir_name", 1, 0, 1); r.Errno != EBADF {
		t.Fatal(r)
	}
	pp, pl := put(m, 1000, "etc")
	call(t, p, m, "path_open", 3, 1, pp, pl, oflagDirectory, rightFdRead, 0, 0, 0)
	fd := uint64(binary.LittleEndian.Uint32(m[0:]))
	if r := call(t, p, m, "fd_fdstat_get", fd, 400); m[400] != ftDirectory || r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_filestat_get", 3, 400); r.Errno != 0 || m[416] != ftDirectory {
		t.Fatal(r)
	}
	// Read in a small buffer, following cookies, collecting names.
	var names []string
	cookie := uint64(0)
	for i := 0; i < 20; i++ {
		if r := call(t, p, m, "fd_readdir", fd, 2000, 512, cookie, 8); r.Errno != 0 {
			t.Fatal(r)
		}
		used := binary.LittleEndian.Uint32(m[8:])
		b := m[2000 : 2000+used]
		for len(b) >= 24 {
			next := binary.LittleEndian.Uint64(b[0:])
			nl := binary.LittleEndian.Uint32(b[16:])
			if uint32(len(b)) < 24+nl {
				break
			}
			names = append(names, string(b[24:24+nl]))
			cookie = next
			b = b[24+nl:]
		}
		if used < 512 {
			break
		}
	}
	if strings.Join(names, ",") != ".,..,a,bb,ccc,motd" {
		t.Fatal(names)
	}
	// A buffer smaller than one entry is filled and truncated, as allowed.
	if r := call(t, p, m, "fd_readdir", fd, 2000, 10, 0, 8); r.Errno != 0 || binary.LittleEndian.Uint32(m[8:]) != 10 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_readdir", fd, 2000, 100, 99, 8); r.Errno != 0 || binary.LittleEndian.Uint32(m[8:]) != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_readdir", 99, 2000, 100, 0, 8); r.Errno != EBADF {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_seek", fd, 0, whenceSet, 8); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_seek", fd, 1, whenceSet, 8); r.Errno != EINVAL {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_read", fd, 100, 1, 8); r.Errno != EISDIR {
		t.Fatal(r)
	}
	if r := call(t, p, m, "fd_filestat_set_times", fd, 0, 0, fstMtimNow); r.Errno != 0 {
		t.Fatal(r)
	}
}

func TestPoll(t *testing.T) {
	p, _, _ := newProc(t)
	m := make(mem, 4096)
	sub := func(i int, userdata uint64, typ byte, a uint32, timeout uint64, flags uint16) {
		s := m[i*48:]
		binary.LittleEndian.PutUint64(s[0:], userdata)
		s[8] = typ
		binary.LittleEndian.PutUint32(s[16:], a)
		binary.LittleEndian.PutUint64(s[24:], timeout)
		binary.LittleEndian.PutUint16(s[40:], flags)
	}
	sub(0, 11, eventClock, 1, uint64(20*time.Millisecond), 0)
	sub(1, 22, eventClock, 1, uint64(5*time.Millisecond), 0)
	r := call(t, p, m, "poll_oneoff", 0, 1000, 2, 3000)
	if r.Errno != 0 || r.Sleep != 5*time.Millisecond || binary.LittleEndian.Uint32(m[3000:]) != 1 || binary.LittleEndian.Uint64(m[1000:]) != 22 {
		t.Fatal(r, binary.LittleEndian.Uint32(m[3000:]))
	}
	// An fd subscription is ready at once: no sleep.
	sub(1, 33, eventFdRead, 0, 0, 0)
	r = call(t, p, m, "poll_oneoff", 0, 1000, 2, 3000)
	if r.Sleep != 0 || binary.LittleEndian.Uint64(m[1000:]) != 33 {
		t.Fatal(r)
	}
	sub(0, 1, eventFdWrite, 99, 0, 0)
	if r = call(t, p, m, "poll_oneoff", 0, 1000, 1, 3000); binary.LittleEndian.Uint16(m[1008:]) != EBADF {
		t.Fatal(r)
	}
	// Absolute realtime in the past: no sleep; bad clock and type: errors.
	sub(0, 1, eventClock, 0, 1, subclockAbstime)
	if r = call(t, p, m, "poll_oneoff", 0, 1000, 1, 3000); r.Sleep != 0 {
		t.Fatal(r)
	}
	sub(0, 1, eventClock, 1, ^uint64(0), subclockAbstime)
	if r = call(t, p, m, "poll_oneoff", 0, 1000, 1, 3000); r.Sleep <= 0 {
		t.Fatal(r)
	}
	sub(0, 1, eventClock, 9, 0, 0)
	if r = call(t, p, m, "poll_oneoff", 0, 1000, 1, 3000); binary.LittleEndian.Uint16(m[1008:]) != EINVAL {
		t.Fatal(r)
	}
	sub(0, 1, 9, 0, 0, 0)
	if r = call(t, p, m, "poll_oneoff", 0, 1000, 1, 3000); binary.LittleEndian.Uint16(m[1008:]) != EINVAL {
		t.Fatal(r)
	}
	for _, n := range []uint64{0, 5000} {
		if r = call(t, p, m, "poll_oneoff", 0, 1000, n, 3000); r.Errno != EINVAL {
			t.Fatal(n, r)
		}
	}
}

// FuzzCall drives every import with arbitrary arguments against a small
// memory: a hostile guest must never make the host panic, allocate without
// bound, or escape its tree.
func callSeeds() []any {
	return []any{
		testutil.Tuple{uint8(FuncIndex["path_open"]), uint64(3), uint64(1), uint64(0), uint64(8), uint64(1), uint64(2), []byte("etc/motd")},
		testutil.Tuple{uint8(FuncIndex["fd_readdir"]), uint64(3), uint64(0), uint64(64), uint64(0), uint64(8), uint64(0), []byte{}},
		testutil.Tuple{uint8(FuncIndex["poll_oneoff"]), uint64(0), uint64(100), uint64(1), uint64(8), uint64(0), uint64(0), make([]byte, 48)},
		testutil.Tuple{uint8(FuncIndex["fd_write"]), uint64(1), uint64(0), uint64(1), uint64(8), uint64(0), uint64(0), []byte{16, 0, 0, 0, 255, 255, 255, 255}},
	}
}

func FuzzCall(f *testing.F) {
	for _, s := range callSeeds() {
		t := s.(testutil.Tuple)
		f.Add(t[0].(uint8), t[1].(uint64), t[2].(uint64), t[3].(uint64), t[4].(uint64), t[5].(uint64), t[6].(uint64), t[7].([]byte))
	}
	f.Fuzz(func(t *testing.T, fn uint8, a0, a1, a2, a3, a4, a5 uint64, memory []byte) {
		p, _, _ := newProc(t)
		m := make(mem, 512)
		copy(m, memory)
		i := int(fn) % len(Funcs)
		args := make([]uint64, len(Funcs[i].Params))
		src := []uint64{a0, a1, a2, a3, a4, a5, a0 ^ a5, a1 ^ a4, a2 ^ a3}
		for j := range args {
			args[j] = src[j]
		}
		p.Call(i, args, m)
	})
}

func TestWriteFuzzCorpus(t *testing.T) {
	testutil.WriteCorpus(t, "FuzzCall", callSeeds()...)
}

func TestCorpusIsCommitted(t *testing.T) {
	ents, err := os.ReadDir(filepath.Join("testdata", "fuzz", "FuzzCall"))
	if err != nil || len(ents) == 0 {
		t.Fatalf("no committed corpus: %v", err)
	}
}
