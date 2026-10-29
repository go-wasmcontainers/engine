package wasi

import (
	"bytes"
	"encoding/binary"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
)

// fakeSpawner records the specs it is asked to start.
type fakeSpawner struct {
	mu    sync.Mutex
	specs []SpawnSpec
	next  uint32
	err   uint32
	kids  []*Child
}

func (s *fakeSpawner) Spawn(parent *Proc, spec SpawnSpec) (*Child, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != 0 {
		spec.FDs.Close(parent)
		return nil, s.err
	}
	s.specs = append(s.specs, spec)
	s.next++
	c := NewChild(100+s.next, nil)
	s.kids = append(s.kids, c)
	spec.FDs.Close(parent)
	return c, 0
}

func wasixProc(t *testing.T, sp Spawner, env ...string) (*Proc, *wasixInst) {
	t.Helper()
	f := rootfs.New()
	for _, d := range []string{"/bin", "/etc"} {
		if err := f.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.WriteFile("/bin/tool.wasm", []byte("\x00asm\x01\x00\x00\x00"), 0o755)
	p := New(Config{FS: f, Spawner: sp, Env: env, Modules: []HostModule{WASIX}})
	return p, WASIX.Open(p).(*wasixInst)
}

// settle runs a Result to its end: waits and resumes.
func settle(t *testing.T, r Result, m Memory) Result {
	t.Helper()
	for r.Wait != nil {
		select {
		case <-r.Wait:
		case <-time.After(5 * time.Second):
			t.Fatal("wait did not end")
		}
		r = r.Resume(m)
	}
	return r
}

// layout is a bump allocator over a test memory.
type layout struct {
	m   mem
	off uint32
}

func newLayout() *layout { return &layout{m: make(mem, 1<<16), off: 64} }

func (l *layout) put(b []byte) uint32 {
	l.off = (l.off + 7) &^ 7
	at := l.off
	copy(l.m[at:], b)
	l.off += uint32(len(b))
	return at
}

func (l *layout) cstrs(list []string) (uint32, uint32) {
	ptrs := make([]byte, 4*len(list))
	for i, s := range list {
		binary.LittleEndian.PutUint32(ptrs[4*i:], l.put(append([]byte(s), 0)))
	}
	if len(list) == 0 {
		return l.put([]byte{0, 0, 0, 0}), 0
	}
	return l.put(ptrs), uint32(len(list))
}

func fdOpBytes(cmd byte, fd, src uint32, pathPtr, pathLen uint32) []byte {
	b := make([]byte, 56)
	b[0] = cmd
	binary.LittleEndian.PutUint32(b[4:], fd)
	binary.LittleEndian.PutUint32(b[8:], src)
	binary.LittleEndian.PutUint32(b[12:], pathPtr)
	binary.LittleEndian.PutUint32(b[16:], pathLen)
	return b
}

func args14(v ...uint32) []uint64 {
	out := make([]uint64, 14)
	for i, x := range v {
		out[i] = uint64(x)
	}
	return out
}

func TestWASIXSpawn3(t *testing.T) {
	sp := &fakeSpawner{}
	p, w := wasixProc(t, sp, "PWD=/", "A=1")
	l := newLayout()
	name := l.put([]byte("tool"))
	av, an := l.cstrs([]string{"tool", "", "a\nb"})
	ev, en := l.cstrs([]string{"X=1", "PWD=/etc"})
	etc := l.put([]byte("/etc"))
	ops := l.put(append(fdOpBytes(wasixOpDup2, 7, 1, 0, 0), fdOpBytes(wasixOpChdir, 0, 0, etc, 4)...))
	path := l.put([]byte("/nowhere:/bin"))
	ret := l.put(make([]byte, 4))
	r := settle(t, w.Call(wfSpawn3, args14(name, 4, av, an, ev, en, ops, 2, 0, 0, 1, path, 13, ret), l.m), l.m)
	if r.Ret != 0 {
		t.Fatalf("errno %d", r.Ret)
	}
	if pid := binary.LittleEndian.Uint32(l.m[ret:]); pid != 101 {
		t.Errorf("pid %d", pid)
	}
	s := sp.specs[0]
	if s.Path != "/bin/tool.wasm" || strings.Join(s.Argv, "|") != "tool||a\nb" || strings.Join(s.Env, " ") != "X=1 PWD=/etc" {
		t.Errorf("spec %+v", s)
	}
	_ = p
}

func TestWASIXSpawn2AndInherit(t *testing.T) {
	sp := &fakeSpawner{}
	_, w := wasixProc(t, sp, "PWD=/", "A=1")
	l := newLayout()
	name := l.put([]byte("/bin/tool.wasm"))
	argv := []byte("tool\nx\n\ny\r\n")
	a := l.put(argv)
	etc := l.put([]byte("/etc"))
	ops := l.put(fdOpBytes(wasixOpChdir, 0, 0, etc, 4))
	ret := l.put(make([]byte, 4))
	r := settle(t, w.Call(wfSpawn2, args14(name, 14, a, uint32(len(argv)), 0, 0, ops, 1, 0, 0, 0, 0, 0, ret), l.m), l.m)
	if r.Ret != 0 {
		t.Fatalf("errno %d", r.Ret)
	}
	s := sp.specs[0]
	if strings.Join(s.Argv, "|") != "tool|x|y" || strings.Join(s.Env, " ") != "A=1 PWD=/etc" {
		t.Errorf("spec %+v", s)
	}
}

func TestWASIXSpawnErrors(t *testing.T) {
	l := newLayout()
	name := l.put([]byte("/bin/tool.wasm"))
	nul := l.put([]byte("a\x00b"))
	av, an := l.cstrs([]string{"tool"})
	badEnv, ben := l.cstrs([]string{"NOEQUALS"})
	ret := l.put(make([]byte, 4))
	badOp := l.put(fdOpBytes(9, 0, 0, 0, 0))
	closeMissing := l.put(fdOpBytes(wasixOpClose, 99, 0, 0, 0))
	openNoPath := l.put(fdOpBytes(wasixOpOpen, 5, 0, 0, 0))
	cases := map[string]struct {
		a    []uint64
		want uint64
	}{
		"empty name":       {args14(name, 0, av, an, 0, 0, 0, 0, 0, 0, 0, 0, 0, ret), EINVAL},
		"name with NUL":    {args14(nul, 3, av, an, 0, 0, 0, 0, 0, 0, 0, 0, 0, ret), EINVAL},
		"name out of mem":  {args14(0xfffffff0, 64, av, an, 0, 0, 0, 0, 0, 0, 0, 0, 0, ret), EFAULT},
		"argv out of mem":  {args14(name, 14, 0xffff0000, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0, ret), EFAULT},
		"too many args":    {args14(name, 14, av, wasixMaxArgs+1, 0, 0, 0, 0, 0, 0, 0, 0, 0, ret), E2BIG},
		"env without =":    {args14(name, 14, av, an, badEnv, ben, 0, 0, 0, 0, 0, 0, 0, ret), EINVAL},
		"bad op":           {args14(name, 14, av, an, 0, 0, badOp, 1, 0, 0, 0, 0, 0, ret), EINVAL},
		"too many ops":     {args14(name, 14, av, an, 0, 0, badOp, wasixMaxFDOps+1, 0, 0, 0, 0, 0, ret), E2BIG},
		"close a hole":     {args14(name, 14, av, an, 0, 0, closeMissing, 1, 0, 0, 0, 0, 0, ret), EBADF},
		"open no path":     {args14(name, 14, av, an, 0, 0, openNoPath, 1, 0, 0, 0, 0, 0, ret), EINVAL},
		"signals too long": {args14(name, 14, av, an, 0, 0, 0, 0, 0, 65, 0, 0, 0, ret), E2BIG},
		"not on PATH":      {args14(nul, 1, av, an, 0, 0, 0, 0, 0, 0, 1, 0, 0, ret), ENOENT},
	}
	for n, c := range cases {
		_, w := wasixProc(t, &fakeSpawner{})
		if r := settle(t, w.Call(wfSpawn3, c.a, l.m), l.m); r.Ret != c.want {
			t.Errorf("%s: errno %d, want %d", n, r.Ret, c.want)
		}
	}
	// spawn2's lists are bounded before they are read.
	_, w := wasixProc(t, &fakeSpawner{})
	if r := w.Call(wfSpawn2, args14(name, 14, 0, MaxArgBytes+1, 0, 0, 0, 0, 0, 0, 0, 0, 0, ret), l.m); r.Ret != E2BIG {
		t.Errorf("spawn2 huge args: %d", r.Ret)
	}
	// The spawner's own errors come back.
	_, w = wasixProc(t, &fakeSpawner{err: EAGAIN})
	if r := settle(t, w.Call(wfSpawn3, args14(name, 14, av, an, 0, 0, 0, 0, 0, 0, 0, 0, 0, ret), l.m), l.m); r.Ret != EAGAIN {
		t.Errorf("spawner error: %d", r.Ret)
	}
}

func TestReadCString(t *testing.T) {
	m := make(mem, 300)
	copy(m[290:], "abcdefghi\x00")
	if s, e := readCString(m, 290, 100); e != 0 || s != "abcdefghi" {
		t.Errorf("%q %d", s, e)
	}
	copy(m[295:], "zzzzz")
	if _, e := readCString(m, 290, 100); e != EFAULT {
		t.Errorf("unterminated at the end of memory: %d", e)
	}
	big := make(mem, 4096)
	for i := range big {
		big[i] = 'x'
	}
	if _, e := readCString(big, 0, 1000); e != E2BIG {
		t.Errorf("too long: %d", e)
	}
	if _, e := readBytes(m, 0xffffffff, 2); e != EFAULT {
		t.Errorf("wrap: %d", e)
	}
}

// fullMem is a 4 GiB memory of 'x' bytes whose address 0 holds a NUL: a
// scan that wrapped from the end of memory would end there.
type fullMem struct{}

func (fullMem) Read(off, n uint32) ([]byte, bool) {
	if uint64(off)+uint64(n) > 1<<32 {
		return nil, false
	}
	b := bytes.Repeat([]byte{'x'}, int(n))
	if off == 0 && n > 0 {
		b[0] = 0
	}
	return b, true
}
func (fullMem) Write(uint32, []byte) bool { return false }

func TestReadCStringDoesNotWrap(t *testing.T) {
	for _, back := range []uint64{1, 10, 256, 300} {
		if s, e := readCString(fullMem{}, uint32(1<<32-back), 1<<20); e != EFAULT {
			t.Errorf("%d bytes before the end: %q, errno %d, want EFAULT", back, s, e)
		}
	}
	if s, e := readCString(fullMem{}, 0, 10); e != 0 || s != "" {
		t.Errorf("at 0: %q %d", s, e)
	}
}

func TestWASIXJoin(t *testing.T) {
	sp := &fakeSpawner{}
	_, w := wasixProc(t, sp)
	l := newLayout()
	name := l.put([]byte("/bin/tool.wasm"))
	av, an := l.cstrs([]string{"tool"})
	ret := l.put(make([]byte, 4))
	for range 2 {
		if r := settle(t, w.Call(wfSpawn3, args14(name, 14, av, an, 0, 0, 0, 0, 0, 0, 0, 0, 0, ret), l.m), l.m); r.Ret != 0 {
			t.Fatal(r.Ret)
		}
	}
	opt := l.put(make([]byte, 8))
	st := l.put(make([]byte, 6))
	setOpt := func(tag byte, pid uint32) {
		clear(l.m[opt : opt+8])
		l.m[opt] = tag
		binary.LittleEndian.PutUint32(l.m[opt+4:], pid)
	}
	// Non-blocking, child running: Nothing.
	setOpt(1, 101)
	if r := w.Call(wfJoin, []uint64{uint64(opt), 1, uint64(st)}, l.m); r.Ret != 0 || l.m[st] != joinNothing {
		t.Fatalf("nonblocking: %d tag %d", r.Ret, l.m[st])
	}
	// Blocking: waits until the child exits with 300.
	r := w.Call(wfJoin, []uint64{uint64(opt), 0, uint64(st)}, l.m)
	if r.Wait == nil {
		t.Fatal("blocking join did not wait")
	}
	sp.kids[0].Exit(ExitStatus{Code: 300}, w.p)
	r = settle(t, r, l.m)
	if r.Ret != 0 || l.m[st] != joinExitNormal || binary.LittleEndian.Uint16(l.m[st+2:]) != 300 || binary.LittleEndian.Uint32(l.m[opt+4:]) != 101 {
		t.Fatalf("exit: %d %v", r.Ret, l.m[st:st+6])
	}
	// Any child, killed: EXIT_SIGNAL 9.
	sp.kids[1].Exit(ExitStatus{Signal: 9}, w.p)
	setOpt(0, 0)
	r = settle(t, w.Call(wfJoin, []uint64{uint64(opt), 0, uint64(st)}, l.m), l.m)
	if r.Ret != 0 || l.m[st] != joinExitSignal || l.m[st+4] != 9 || binary.LittleEndian.Uint32(l.m[opt+4:]) != 102 {
		t.Fatalf("signal: %d %v", r.Ret, l.m[st:st+6])
	}
	// No child left.
	if r := w.Call(wfJoin, []uint64{uint64(opt), 0, uint64(st)}, l.m); r.Ret != ECHILD {
		t.Errorf("no child: %d", r.Ret)
	}
	for _, bad := range []func(){func() { setOpt(2, 0) }, func() { setOpt(1, 0) }} {
		bad()
		if r := w.Call(wfJoin, []uint64{uint64(opt), 0, uint64(st)}, l.m); r.Ret != EINVAL {
			t.Errorf("bad option: %d", r.Ret)
		}
	}
	if r := w.Call(wfJoin, []uint64{0xfffffffc, 0, uint64(st)}, l.m); r.Ret != EFAULT {
		t.Errorf("bad pointer: %d", r.Ret)
	}
}

func TestWASIXIdsPipeTty(t *testing.T) {
	p, w := wasixProc(t, &fakeSpawner{})
	l := newLayout()
	out := l.put(make([]byte, 24))
	if r := w.Call(wfProcID, []uint64{uint64(out)}, l.m); r.Ret != 0 || binary.LittleEndian.Uint32(l.m[out:]) != p.Pid() {
		t.Errorf("proc_id %d", r.Ret)
	}
	if r := w.Call(wfProcParent, []uint64{uint64(p.Pid()), uint64(out)}, l.m); r.Ret != 0 || binary.LittleEndian.Uint32(l.m[out:]) != 0 {
		t.Errorf("proc_parent self %d", r.Ret)
	}
	if r := w.Call(wfProcParent, []uint64{999, uint64(out)}, l.m); r.Ret != ESRCH {
		t.Errorf("proc_parent unknown %d", r.Ret)
	}
	if r := w.Call(wfFdPipe, []uint64{uint64(out), uint64(out + 4)}, l.m); r.Ret != 0 {
		t.Errorf("fd_pipe %d", r.Ret)
	}
	if r := w.Call(wfFdPipe, []uint64{0xfffffffe, 0}, l.m); r.Ret != EFAULT {
		t.Errorf("fd_pipe bad ptr %d", r.Ret)
	}
	if r := w.Call(wfTtySet, []uint64{uint64(out)}, l.m); r.Ret != ENOTTY {
		t.Errorf("tty_set without terminal %d", r.Ret)
	}
	term := NewTerminal(80, 24, [3]bool{true, true, true})
	p2 := New(Config{FS: rootfs.New(), Terminal: term})
	w2 := WASIX.Open(p2).(*wasixInst)
	clear(l.m[out : out+24])
	if r := w2.Call(wfTtySet, []uint64{uint64(out)}, l.m); r.Ret != 0 {
		t.Fatalf("tty_set %d", r.Ret)
	}
	if echo, line := term.Mode(); echo || line {
		t.Error("tty_set did not turn echo and line mode off")
	}
	if r := w2.Call(wfTtyGet, []uint64{uint64(out)}, l.m); r.Ret != 0 || l.m[out] != 80 || l.m[out+16] != 1 {
		t.Errorf("tty_get %d %v", r.Ret, l.m[out:out+24])
	}
	if r := w.Call(99, nil, l.m); r.Trap == "" {
		t.Error("unknown function did not trap")
	}
	if _, err := Imports([]HostModule{WASIX}); err != nil {
		t.Errorf("WASIX is not a valid host module: %v", err)
	}
}

// FuzzWASIX feeds every WASIX call guest memory and arguments it
// chooses: the host must answer an errno or wait, never panic.
func FuzzWASIX(f *testing.F) {
	f.Add(uint8(wfSpawn3), []byte("tool\x00"), uint32(0), uint32(4), uint32(8), uint32(1))
	f.Add(uint8(wfSpawn2), []byte("/bin/tool.wasm\ntool\n"), uint32(0), uint32(14), uint32(0), uint32(20))
	f.Add(uint8(wfJoin), []byte{1, 0, 0, 0, 101, 0, 0, 0}, uint32(0), uint32(0), uint32(16), uint32(0))
	f.Fuzz(func(t *testing.T, fn uint8, data []byte, a0, a1, a2, a3 uint32) {
		if len(data) > 4096 {
			return
		}
		m := make(mem, 8192)
		copy(m, data)
		_, w := wasixProc(t, &fakeSpawner{})
		n := len(WASIX.Funcs())
		k := int(fn) % (n + 1)
		args := make([]uint64, 14)
		for i, v := range []uint32{a0, a1, a2, a3, a0 ^ a1, a2 ^ a3, a1, a2 % 300, 0, a3 % 70, a0 & 1, a1, a2 % 128, a3} {
			args[i] = uint64(v)
		}
		if k < n {
			args = args[:len(WASIX.Funcs()[k].Params)]
		}
		r := w.Call(k, args, m)
		if r.Wait != nil && k != wfJoin {
			select {
			case <-r.Wait:
				r.Resume(m)
			case <-time.After(5 * time.Second):
				t.Fatal("spawn wait did not end")
			}
		}
	})
}
