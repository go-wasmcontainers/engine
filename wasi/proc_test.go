package wasi

import (
	"encoding/binary"
	"runtime"
	"slices"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
)

// iov writes one iovec at off pointing at buf..buf+n.
func iov(m mem, off, buf, n uint32) {
	binary.LittleEndian.PutUint32(m[off:], buf)
	binary.LittleEndian.PutUint32(m[off+4:], n)
}

func TestPipeSemantics(t *testing.T) {
	p, _, _ := newProc(t)
	m := make(mem, 1<<17)
	r, w, e := p.Pipe()
	if e != 0 {
		t.Fatal(e)
	}
	// Read on an empty pipe blocks (a Wait), then sees the data written.
	iov(m, 0, 100, 10)
	res := call(t, p, m, "fd_read", uint64(r), 0, 1, 50)
	if res.Wait == nil {
		t.Fatalf("empty read did not wait: %+v", res)
	}
	put(m, 200, "abc")
	iov(m, 8, 200, 3)
	if x := call(t, p, m, "fd_write", uint64(w), 8, 1, 60); x.Errno != 0 || binary.LittleEndian.Uint32(m[60:]) != 3 {
		t.Fatalf("write: %+v", x)
	}
	select {
	case <-res.Wait:
	case <-time.After(time.Second):
		t.Fatal("the write did not wake the reader")
	}
	if x := res.Resume(m); x.Errno != 0 || binary.LittleEndian.Uint32(m[50:]) != 3 || string(m[100:103]) != "abc" {
		t.Fatalf("resumed read: %+v %q", x, m[100:103])
	}
	// Non-blocking read of an empty pipe: EAGAIN.
	call(t, p, m, "fd_fdstat_set_flags", uint64(r), fdflagNonblock)
	if x := call(t, p, m, "fd_read", uint64(r), 0, 1, 50); x.Errno != EAGAIN {
		t.Fatalf("nonblocking read: %+v", x)
	}
	if x := call(t, p, m, "fd_fdstat_get", uint64(r), 300); x.Errno != 0 || m[300] != ftUnknown || binary.LittleEndian.Uint16(m[302:]) != fdflagNonblock {
		t.Fatalf("fdstat: %+v %v", x, m[300:304])
	}
	if x := call(t, p, m, "fd_filestat_get", uint64(w), 300); x.Errno != 0 {
		t.Fatalf("filestat: %+v", x)
	}
	// A write end dup'ed: closing one leaves the other; closing both is
	// end of file.
	w2, e := p.Dup(w, 10)
	if e != 0 || w2 != 10 {
		t.Fatal(w2, e)
	}
	call(t, p, m, "fd_close", uint64(w))
	if ready, hup, _, _ := p.fds[r].pipe.readiness(false); ready || hup {
		t.Fatal("end of file with a writer left")
	}
	call(t, p, m, "fd_close", uint64(w2))
	if x := call(t, p, m, "fd_read", uint64(r), 0, 1, 50); x.Errno != 0 || binary.LittleEndian.Uint32(m[50:]) != 0 {
		t.Fatalf("read at end of file: %+v", x)
	}
	// No reader left: EPIPE.
	r2, w3, _ := p.Pipe()
	call(t, p, m, "fd_close", uint64(r2))
	if x := call(t, p, m, "fd_write", uint64(w3), 8, 1, 60); x.Errno != EPIPE {
		t.Fatalf("write with no reader: %+v", x)
	}
	// Wrong direction.
	if x := call(t, p, m, "fd_write", uint64(r), 8, 1, 60); x.Errno != EBADF {
		t.Fatalf("write on a read end: %+v", x)
	}
	// Positional I/O: ESPIPE.
	r3, _, _ := p.Pipe()
	if x := call(t, p, m, "fd_pread", uint64(r3), 0, 1, 0, 50); x.Errno != ESPIPE {
		t.Fatalf("pread: %+v", x)
	}
}

func TestPipeFullBlocksWriter(t *testing.T) {
	p, _, _ := newProc(t)
	m := make(mem, PipeSize+4096)
	r, w, _ := p.Pipe()
	iov(m, 0, 1024, PipeSize)
	if x := call(t, p, m, "fd_write", uint64(w), 0, 1, 16); x.Errno != 0 || binary.LittleEndian.Uint32(m[16:]) != PipeSize {
		t.Fatalf("fill: %+v", x)
	}
	res := call(t, p, m, "fd_write", uint64(w), 0, 1, 16)
	if res.Wait == nil {
		t.Fatalf("a full pipe did not block the writer: %+v", res)
	}
	iov(m, 8, 1024, 100)
	call(t, p, m, "fd_read", uint64(r), 8, 1, 20)
	select {
	case <-res.Wait:
	case <-time.After(time.Second):
		t.Fatal("a read did not wake the writer")
	}
	if x := res.Resume(m); x.Errno != 0 || binary.LittleEndian.Uint32(m[16:]) != 100 {
		t.Fatalf("resumed write: %+v %d", x, binary.LittleEndian.Uint32(m[16:]))
	}
}

// sub writes a poll_oneoff subscription at off.
func sub(m mem, off uint32, userdata uint64, typ uint8, fd uint32, timeout uint64) {
	clear(m[off : off+48])
	binary.LittleEndian.PutUint64(m[off:], userdata)
	m[off+8] = typ
	if typ == eventClock {
		binary.LittleEndian.PutUint64(m[off+24:], timeout)
	} else {
		binary.LittleEndian.PutUint32(m[off+16:], fd)
	}
}

func TestPollPipes(t *testing.T) {
	p, _, _ := newProc(t)
	m := make(mem, 4096)
	r, w, _ := p.Pipe()
	// Writable at once; not readable: the write end is reported.
	sub(m, 0, 1, eventFdRead, r, 0)
	sub(m, 48, 2, eventFdWrite, w, 0)
	if x := call(t, p, m, "poll_oneoff", 0, 1000, 2, 2000); x.Errno != 0 || binary.LittleEndian.Uint32(m[2000:]) != 1 || binary.LittleEndian.Uint64(m[1000:]) != 2 {
		t.Fatalf("poll: %+v n=%d", x, binary.LittleEndian.Uint32(m[2000:]))
	}
	// Read end with a clock: the poll waits, then the clock fires.
	sub(m, 0, 1, eventFdRead, r, 0)
	sub(m, 48, 3, eventClock, 0, uint64(20*time.Millisecond))
	x := call(t, p, m, "poll_oneoff", 0, 1000, 2, 2000)
	if x.Wait == nil || x.Sleep <= 0 {
		t.Fatalf("poll on an empty pipe with a clock: %+v", x)
	}
	time.Sleep(x.Sleep)
	if y := x.Resume(m); y.Errno != 0 || binary.LittleEndian.Uint64(m[1000:]) != 3 {
		t.Fatalf("clock after the wait: %+v ud=%d", y, binary.LittleEndian.Uint64(m[1000:]))
	}
	// An expired clock is reported with a ready descriptor.
	sub(m, 0, 2, eventFdWrite, w, 0)
	sub(m, 48, 3, eventClock, 0, 0)
	if y := call(t, p, m, "poll_oneoff", 0, 1000, 2, 2000); y.Errno != 0 || binary.LittleEndian.Uint32(m[2000:]) != 2 {
		t.Fatalf("expired clock dropped: %+v n=%d", y, binary.LittleEndian.Uint32(m[2000:]))
	}
	// Closing the write end makes the read end ready (hang-up).
	call(t, p, m, "fd_close", uint64(w))
	sub(m, 0, 1, eventFdRead, r, 0)
	if y := call(t, p, m, "poll_oneoff", 0, 1000, 1, 2000); y.Errno != 0 || binary.LittleEndian.Uint16(m[1000+24:]) != eventHangup {
		t.Fatalf("hang-up: %+v", y)
	}
}

// TestPollPipeNoGoroutineLeak: timed polls on an idle pipe leave no
// goroutines behind (a guest looping on poll(pipe, 1µs) must not grow the
// host). Before the fix: two goroutines per poll, 1000 after 500 polls.
func TestPollPipeNoGoroutineLeak(t *testing.T) {
	p, _, _ := newProc(t)
	m := make(mem, 4096)
	r, _, _ := p.Pipe()
	settle := func() int {
		time.Sleep(50 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	before := settle()
	const polls = 500
	waited := 0
	for i := 0; i < polls; i++ {
		sub(m, 0, 1, eventFdRead, r, 0)
		sub(m, 48, 3, eventClock, 0, uint64(time.Microsecond))
		x := call(t, p, m, "poll_oneoff", 0, 1000, 2, 2000)
		if x.Wait != nil {
			waited++
			time.Sleep(x.Sleep)
			x.Resume(m)
		}
	}
	if waited == 0 {
		t.Fatal("no poll waited: the test measured nothing")
	}
	if d := settle() - before; d > 20 {
		t.Fatalf("%d goroutines left after %d timed polls", d, polls)
	}
}

func TestGroupKillThenRegister(t *testing.T) {
	g := NewGroup(GroupLimits{})
	p := New(Config{FS: rootfs.New(), Group: g})
	if e := g.Admit(0); e != 0 {
		t.Fatal(e)
	}
	c := New(Config{FS: rootfs.New(), Group: g, Admitted: true, Parent: p})
	g.Kill()
	killed := false
	g.OnKill(c.Pid(), func() { killed = true })
	if !killed {
		t.Error("a process registered after the group was killed was not killed")
	}
}

type countMod struct{ closes *int }

func (countMod) Module() string        { return "count" }
func (countMod) Funcs() []HostFunc     { return []HostFunc{{Name: "f"}} }
func (c countMod) Open(*Proc) Instance { return countInst(c) }

type countInst countMod

func (countInst) Call(int, []uint64, Memory) Result { return Result{} }
func (c countInst) Close()                          { *c.closes++ }

func TestCloseTwiceClosesInstancesOnce(t *testing.T) {
	n := 0
	p := New(Config{FS: rootfs.New(), Modules: []HostModule{countMod{&n}}})
	p.CallImport(len(Funcs), nil, make(mem, 8))
	p.Close()
	p.Close()
	if n != 1 {
		t.Errorf("instance closed %d times", n)
	}
}

func TestReadModuleBounded(t *testing.T) {
	f := rootfs.New()
	big := make([]byte, MaxModuleBytes+1)
	copy(big, "\x00asm\x01\x00\x00\x00")
	f.WriteFile("/big.wasm", big, 0o644)
	p := New(Config{FS: f})
	if _, e := p.ReadModule("/big.wasm"); e != ENOEXEC {
		t.Errorf("oversized module: %d", e)
	}
}

func TestCloneFDs(t *testing.T) {
	f := rootfs.New()
	f.MkdirAll("/etc/sub", 0o755)
	f.WriteFile("/etc/motd", []byte("hello"), 0o644)
	vol := rootfs.New()
	vol.MkdirAll("/d", 0o755)
	p := New(Config{FS: f, Mounts: []Mount{{Path: "/data", FS: vol}}})
	m := make(mem, 1024)
	r, w, _ := p.Pipe()
	p.SetCloexec(r, true)
	ops := []FDOp{
		{Kind: FDDup2, FD: w, To: 1},
		{Kind: FDClose, FD: w},
		{Kind: FDClose, FD: 3}, // a preopen: ignored
		{Kind: FDOpen, FD: 7, Path: "motd", OFlags: 0},
		{Kind: FDChdir, Path: "sub"},
	}
	tb, e := p.CloneFDs("/etc", ops)
	if e != 0 {
		t.Fatal(e)
	}
	if tb.Cwd != "/etc/sub" {
		t.Errorf("cwd %q", tb.Cwd)
	}
	if _, ok := tb.fds[r]; ok {
		t.Error("a cloexec descriptor reached the child")
	}
	if _, ok := tb.fds[w]; ok {
		t.Error("a closed descriptor reached the child")
	}
	if tb.fds[1] == nil || tb.fds[1].pipe == nil || tb.fds[3] == nil || tb.fds[7] == nil || tb.fds[7].file == nil {
		t.Fatalf("child table: %v", slices.Sorted(func(yield func(uint32) bool) {
			for k := range tb.fds {
				if !yield(k) {
					return
				}
			}
		}))
	}
	// The child's copy holds the write end: the parent closing its own
	// does not end the pipe.
	call(t, p, m, "fd_close", uint64(w))
	if _, hup, _, _ := p.fds[r].pipe.readiness(false); hup {
		t.Error("end of file while the child's copy is open")
	}
	child := New(Config{FS: f, FDs: tb, Parent: p, Group: p.Group()})
	if child.Pid() != 2 || child.Parent() != p {
		t.Errorf("pid %d", child.Pid())
	}
	child.Close()
	if _, hup, _, _ := p.fds[r].pipe.readiness(false); !hup {
		t.Error("no end of file after the child closed")
	}
	// Errors.
	for name, op := range map[string]FDOp{
		"dup2 onto a preopen": {Kind: FDDup2, FD: 0, To: 3},
		"dup2 of nothing":     {Kind: FDDup2, FD: 99, To: 5},
		"open missing":        {Kind: FDOpen, FD: 8, Path: "/nope"},
		"chdir to a file":     {Kind: FDChdir, Path: "/etc/motd"},
		"fchdir a pipe":       {Kind: FDFchdir, FD: r},
	} {
		if _, e := p.CloneFDs("/", []FDOp{op}); e == 0 {
			t.Errorf("%s accepted", name)
		}
	}
	// fchdir to a directory opened under a mount resolves to its guest path.
	tb2, e := p.CloneFDs("/", []FDOp{{Kind: FDOpen, FD: 9, Path: "/data/d", OFlags: oflagDirectory}, {Kind: FDFchdir, FD: 9}})
	if e != 0 || tb2.Cwd != "/data/d" {
		t.Errorf("fchdir under a mount: %q %d", tb2.Cwd, e)
	}
}

func TestDup2Flags(t *testing.T) {
	p, _, _ := newProc(t)
	r, _, _ := p.Pipe()
	p.fds[r].nonblock = true
	p.SetCloexec(r, true)
	if e := p.Dup2(r, 20); e != 0 {
		t.Fatal(e)
	}
	if !p.fds[20].nonblock || p.fds[20].cloexec {
		t.Error("dup2 must copy nonblock and clear cloexec")
	}
	if e := p.Dup2(r, 3); e != EBADF {
		t.Errorf("dup2 onto a preopen: %d", e)
	}
	m := make(mem, 64)
	if x := call(t, p, m, "fd_fdstat_get", 1, 0); x.Errno != 0 || m[0] != ftCharDev || binary.LittleEndian.Uint16(m[2:]) != 0 {
		t.Errorf("stdout fdstat %v", m[:4])
	}
	p.fds[1].nonblock = true
	if call(t, p, m, "fd_fdstat_get", 1, 0); binary.LittleEndian.Uint16(m[2:]) != fdflagNonblock {
		t.Errorf("stdout nonblock not reported: %v", m[:4])
	}
}

func TestGroupLimits(t *testing.T) {
	g := NewGroup(GroupLimits{MaxProcs: 3, MaxMemoryBytes: 300, SpawnRate: 1e-9, SpawnBurst: 2})
	f := rootfs.New()
	root := New(Config{FS: f, Group: g, MemoryLimitBytes: 100})
	if root.Pid() != 1 {
		t.Fatal(root.Pid())
	}
	if e := g.Admit(300); e != ENOMEM {
		t.Errorf("memory: %d", e)
	}
	if e := g.Admit(100); e != 0 {
		t.Fatal(e)
	}
	c := New(Config{FS: f, Group: g, MemoryLimitBytes: 100, Admitted: true, Parent: root})
	// One token left of the burst of 2: one more admission, then EAGAIN.
	if e := g.Admit(10); e != 0 {
		t.Fatalf("second admission: %d", e)
	}
	g.Cancel(10)
	if e := g.Admit(10); e != EAGAIN {
		t.Errorf("spawn rate: %d", e)
	}
	// The process count, with tokens to spare.
	g2 := NewGroup(GroupLimits{MaxProcs: 1})
	New(Config{FS: f, Group: g2})
	if e := g2.Admit(0); e != EAGAIN {
		t.Errorf("process count: %d", e)
	}
	killed := 0
	g.OnKill(c.Pid(), func() { killed++ })
	if pids := g.Pids(); pids[2] != 1 || len(pids) != 2 {
		t.Errorf("pids %v", pids)
	}
	g.Kill()
	if killed != 1 || g.Admit(1) != ESRCH {
		t.Errorf("kill: %d", killed)
	}
	c.Close()
	c.Close() // twice is harmless
	if g.Len() != 1 {
		t.Errorf("len %d", g.Len())
	}
}

func TestReap(t *testing.T) {
	p, _, _ := newProc(t)
	if _, e := p.Reap(0); e != ECHILD {
		t.Fatal(e)
	}
	c := NewChild(5, nil)
	p.children = append(p.children, c)
	if _, e := p.Reap(0); e != EAGAIN {
		t.Fatal(e)
	}
	if _, e := p.Reap(6); e != ECHILD {
		t.Fatal(e)
	}
	wake := p.ChildChanged()
	c.Exit(ExitStatus{Code: 300}, p)
	select {
	case <-wake:
	default:
		t.Fatal("no wake on a child's exit")
	}
	got, e := p.Reap(0)
	if e != 0 || got != c || got.Status().Code != 300 {
		t.Fatal(got, e)
	}
	if _, e := p.Reap(5); e != ECHILD {
		t.Fatal("reaped twice")
	}
}

func TestLookPathAndTerminal(t *testing.T) {
	f := rootfs.New()
	f.MkdirAll("/bin", 0o755)
	f.WriteFile("/bin/ls.wasm", []byte("\x00asm\x01\x00\x00\x00"), 0o755)
	f.WriteFile("/bin/cat", []byte("\x00asm\x01\x00\x00\x00"), 0o755)
	f.WriteFile("/bin/notwasm", []byte("#!/bin/sh"), 0o755)
	term := NewTerminal(80, 24, [3]bool{true, true, false})
	p := New(Config{FS: f, Terminal: term})
	for name, want := range map[string]string{"ls": "/bin/ls.wasm", "cat": "/bin/cat", "./bin/cat": "/bin/cat", "/bin/ls.wasm": "/bin/ls.wasm"} {
		if got, e := p.LookPath(name, "/usr/bin:/bin", "/"); e != 0 || got != want {
			t.Errorf("LookPath(%q) = %q, %d; want %q", name, got, e, want)
		}
	}
	if _, e := p.LookPath("nope", "/bin", "/"); e != ENOENT {
		t.Error("nope found")
	}
	if _, e := p.ReadModule("/bin/notwasm"); e != ENOEXEC {
		t.Errorf("ReadModule of a script: %d", e)
	}
	if _, e := p.ReadModule("/bin"); e != ENOEXEC {
		t.Errorf("ReadModule of a directory: %d", e)
	}
	if !p.IsTerminal(0) || !p.IsTerminal(1) || p.IsTerminal(2) || p.IsTerminal(3) {
		t.Error("IsTerminal")
	}
	term.SetSize(100, 30)
	term.SetMode(false, false)
	if c, r := p.Terminal().Size(); c != 100 || r != 30 {
		t.Error("size")
	}
	if e, l := p.Terminal().Mode(); e || l {
		t.Error("mode")
	}
}
