package wasi

import (
	"encoding/binary"
	"strings"
)

// WASIX is the subset of WASIX (https://wasix.org) that lets a guest start
// and wait for other programs of its container, served as the host module
// "wasix_32v1": proc_spawn2, proc_spawn3, proc_join, proc_id,
// proc_parent, fd_pipe, tty_get and tty_set. Nothing else of WASIX (no
// fork, exec, threads or signals).
//
// Signatures and layouts are wasix-libc's (api_wasix.h and
// __wasixlibc_real.c: every argument an i32, the errno as the result);
// the behaviour follows wasmer's implementation where it is sound, and
// departs from it where wasmer is wrong (see the go-wasmcontainers/shell
// repository's docs/REFERENCES.md): exit codes travel raw, fd flags are
// the descriptor's own, pipe ends have a filestat and a readiness.
var WASIX HostModule = wasixModule{}

// WASIXModule is the import module name.
const WASIXModule = "wasix_32v1"

// Bounds on what a spawn reads from the guest; MaxArgBytes (Spawn's
// E2BIG) bounds argv and env together.
const (
	wasixMaxName   = 4096
	wasixMaxPath   = 64 << 10
	wasixMaxFDOps  = 256
	wasixMaxArgs   = 1 << 14
	wasixFDOpSize  = 56
	wasixRightsFDW = 1 << 6 // __WASI_RIGHTS_FD_WRITE
)

// fd op names (__wasi_proc_spawn_fd_op_name_t).
const (
	wasixOpClose = iota
	wasixOpDup2
	wasixOpOpen
	wasixOpChdir
	wasixOpFchdir
)

// join status tags (__wasi_join_status_type_t) and flags.
const (
	joinNothing    = 0
	joinExitNormal = 1
	joinExitSignal = 2

	joinNonBlocking = 1
)

const (
	wfFdPipe = iota
	wfSpawn2
	wfSpawn3
	wfJoin
	wfProcID
	wfProcParent
	wfTtyGet
	wfTtySet
)

type wasixModule struct{}

func (wasixModule) Module() string { return WASIXModule }

func (wasixModule) Funcs() []HostFunc {
	const i = "i"
	return []HostFunc{
		wfFdPipe:     {Name: "fd_pipe", Params: "ii", Result: i},
		wfSpawn2:     {Name: "proc_spawn2", Params: strings.Repeat(i, 14), Result: i},
		wfSpawn3:     {Name: "proc_spawn3", Params: strings.Repeat(i, 14), Result: i},
		wfJoin:       {Name: "proc_join", Params: "iii", Result: i},
		wfProcID:     {Name: "proc_id", Params: "i", Result: i},
		wfProcParent: {Name: "proc_parent", Params: "ii", Result: i},
		wfTtyGet:     {Name: "tty_get", Params: "i", Result: i},
		wfTtySet:     {Name: "tty_set", Params: "i", Result: i},
	}
}

func (wasixModule) Open(p *Proc) Instance { return &wasixInst{p: p} }

type wasixInst struct{ p *Proc }

func (*wasixInst) Close() {}

func errnoRet(e uint32) Result { return Result{Ret: uint64(e)} }

func (w *wasixInst) Call(fn int, a []uint64, m Memory) Result {
	arg := func(k int) uint32 { return uint32(a[k]) }
	switch fn {
	case wfFdPipe:
		r, wr, e := w.p.Pipe()
		if e != 0 {
			return errnoRet(e)
		}
		if !writeU32(m, arg(0), r) || !writeU32(m, arg(1), wr) {
			return errnoRet(EFAULT)
		}
		return errnoRet(0)
	case wfSpawn2, wfSpawn3:
		return w.spawn(fn == wfSpawn3, a, m)
	case wfJoin:
		return w.join(arg(0), arg(1), arg(2), m)
	case wfProcID:
		if !writeU32(m, arg(0), w.p.Pid()) {
			return errnoRet(EFAULT)
		}
		return errnoRet(0)
	case wfProcParent:
		return w.parent(arg(0), arg(1), m)
	case wfTtyGet:
		return w.ttyGet(arg(0), m)
	case wfTtySet:
		return w.ttySet(arg(0), m)
	}
	return Result{Trap: "wasix_32v1: no such function"}
}

// readBytes reads n bytes at off, refusing a range past 4 GiB.
func readBytes(m Memory, off, n uint32) ([]byte, uint32) {
	if uint64(off)+uint64(n) > 1<<32 {
		return nil, EFAULT
	}
	if n == 0 {
		return nil, 0
	}
	b, ok := m.Read(off, n)
	if !ok {
		return nil, EFAULT
	}
	return append([]byte(nil), b...), 0
}

// readCString reads a NUL-terminated string at off, of at most max bytes.
// The scan position is 64-bit: a string that runs to the last byte of a
// 4 GiB memory is EFAULT, it never wraps around to address 0.
func readCString(m Memory, off uint32, max int) (string, uint32) {
	var out []byte
	for pos := uint64(off); len(out) <= max; {
		if pos >= 1<<32 {
			return "", EFAULT
		}
		chunk := min(uint64(256), 1<<32-pos)
		b, ok := m.Read(uint32(pos), uint32(chunk))
		if !ok {
			// Near the end of memory: one byte at a time.
			b, ok = m.Read(uint32(pos), 1)
			if !ok {
				return "", EFAULT
			}
		}
		if i := indexZero(b); i >= 0 {
			out = append(out, b[:i]...)
			if len(out) > max {
				break
			}
			return string(out), 0
		}
		out = append(out, b...)
		pos += uint64(len(b))
	}
	return "", E2BIG
}

func indexZero(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

// readStringArray reads proc_spawn3's `const uint8_t **` list: n 32-bit
// pointers to NUL-terminated strings, *budget bytes at most in all.
func readStringArray(m Memory, ptr, n uint32, budget *int) ([]string, uint32) {
	if n > wasixMaxArgs {
		return nil, E2BIG
	}
	raw, e := readBytes(m, ptr, n*4)
	if e != 0 {
		return nil, e
	}
	out := make([]string, 0, n)
	for i := uint32(0); i < n; i++ {
		s, e := readCString(m, binary.LittleEndian.Uint32(raw[4*i:]), *budget)
		if e != 0 {
			return nil, e
		}
		*budget -= len(s) + 1
		if *budget < 0 {
			return nil, E2BIG
		}
		out = append(out, s)
	}
	return out, 0
}

// splitDelimited is proc_spawn2's list: entries separated by line feeds
// (or carriage returns), empty entries dropped — as wasmer parses it,
// which is why a shell should call proc_spawn3.
func splitDelimited(s string) []string {
	var out []string
	for _, f := range strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' }) {
		out = append(out, f)
	}
	return out
}

// decodeFDOps decodes n __wasi_proc_spawn_fd_op_t (56 bytes each) at ptr.
func decodeFDOps(m Memory, ptr, n uint32) ([]FDOp, uint32) {
	if n == 0 {
		return nil, 0
	}
	if n > wasixMaxFDOps {
		return nil, E2BIG
	}
	raw, e := readBytes(m, ptr, n*wasixFDOpSize)
	if e != 0 {
		return nil, e
	}
	ops := make([]FDOp, 0, n)
	for i := uint32(0); i < n; i++ {
		op, e := decodeFDOp(m, raw[i*wasixFDOpSize:(i+1)*wasixFDOpSize])
		if e != 0 {
			return nil, e
		}
		ops = append(ops, op)
	}
	return ops, 0
}

// decodeFDOp decodes one fd op: cmd u8 @0, fd u32 @4, src_fd u32 @8,
// path ptr @12, path_len @16, dirflags @20, oflags u16 @24,
// fs_rights_base u64 @32, fs_rights_inheriting u64 @40, fdflags u16 @48,
// fdflagsext u16 @50.
func decodeFDOp(m Memory, b []byte) (FDOp, uint32) {
	le := binary.LittleEndian
	fd, src := le.Uint32(b[4:]), le.Uint32(b[8:])
	path := func() (string, uint32) {
		n := le.Uint32(b[16:])
		if n == 0 || n > wasixMaxPath {
			return "", EINVAL
		}
		p, e := readBytes(m, le.Uint32(b[12:]), n)
		if e != 0 {
			return "", e
		}
		if indexZero(p) >= 0 {
			return "", EINVAL
		}
		return string(p), 0
	}
	switch b[0] {
	case wasixOpClose:
		return FDOp{Kind: FDClose, FD: fd}, 0
	case wasixOpDup2:
		return FDOp{Kind: FDDup2, FD: src, To: fd}, 0
	case wasixOpOpen:
		p, e := path()
		if e != 0 {
			return FDOp{}, e
		}
		return FDOp{Kind: FDOpen, FD: fd, Path: p, OFlags: uint32(le.Uint16(b[24:])),
			FDFlags: uint32(le.Uint16(b[48:])), Write: le.Uint64(b[32:])&wasixRightsFDW != 0}, 0
	case wasixOpChdir:
		p, e := path()
		if e != 0 {
			return FDOp{}, e
		}
		return FDOp{Kind: FDChdir, Path: p}, 0
	case wasixOpFchdir:
		return FDOp{Kind: FDFchdir, FD: fd}, 0
	}
	return FDOp{}, EINVAL
}

// envGet returns the value of key in a KEY=value list.
func envGet(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// spawn implements proc_spawn2 (lists as line-feed separated strings) and
// proc_spawn3 (lists as arrays of C strings). Arguments, in order: name,
// name_len, args, args_len, envs, envs_len, fd_ops, fd_ops_len, signals,
// signals_len, search_path, path, path_len, ret_pid.
func (w *wasixInst) spawn(v3 bool, a []uint64, m Memory) Result {
	arg := func(k int) uint32 { return uint32(a[k]) }
	p := w.p
	if arg(1) == 0 || arg(1) > wasixMaxName {
		return errnoRet(EINVAL)
	}
	nameB, e := readBytes(m, arg(0), arg(1))
	if e != 0 {
		return errnoRet(e)
	}
	name := string(nameB)
	if indexZero(nameB) >= 0 {
		return errnoRet(EINVAL)
	}
	budget := MaxArgBytes
	var argv, env []string
	inherit := arg(4) == 0
	if v3 {
		if argv, e = readStringArray(m, arg(2), arg(3), &budget); e != 0 {
			return errnoRet(e)
		}
		if !inherit {
			if env, e = readStringArray(m, arg(4), arg(5), &budget); e != 0 {
				return errnoRet(e)
			}
		}
	} else {
		if arg(3) > uint32(budget) || (!inherit && arg(5) > uint32(budget)-arg(3)) {
			return errnoRet(E2BIG)
		}
		s, e := readBytes(m, arg(2), arg(3))
		if e != 0 {
			return errnoRet(e)
		}
		argv = splitDelimited(string(s))
		if !inherit {
			s, e := readBytes(m, arg(4), arg(5))
			if e != 0 {
				return errnoRet(e)
			}
			env = splitDelimited(string(s))
		}
	}
	if inherit {
		env = append([]string(nil), p.cfg.Env...)
	}
	for _, kv := range env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" || strings.ContainsRune(kv, 0) {
			return errnoRet(EINVAL)
		}
	}
	for _, s := range argv {
		if strings.ContainsRune(s, 0) {
			return errnoRet(EINVAL)
		}
	}
	ops, e := decodeFDOps(m, arg(6), arg(7))
	if e != 0 {
		return errnoRet(e)
	}
	// Signal dispositions (arg 8, 9) are ignored: signals are not part of
	// the subset. Their length is still bounded.
	if arg(9) > 64 {
		return errnoRet(E2BIG)
	}
	searchPath := arg(10) != 0
	pathList := "/usr/local/bin:/bin:/usr/bin"
	if arg(11) != 0 && arg(12) != 0 {
		if arg(12) > wasixMaxPath {
			return errnoRet(E2BIG)
		}
		pb, e := readBytes(m, arg(11), arg(12))
		if e != 0 {
			return errnoRet(e)
		}
		pathList = strings.TrimRight(string(pb), "\x00")
	}
	retPid := arg(13)
	// The child starts in the parent's directory (its $PWD: a wasip1
	// guest keeps its working directory there), moved by the CHDIR ops.
	cwd, _ := envGet(p.cfg.Env, "PWD")
	if !strings.HasPrefix(cwd, "/") {
		cwd = "/"
	}
	t, e := p.CloneFDs(cwd, ops)
	if e != 0 {
		return errnoRet(e)
	}
	var abs string
	if searchPath && !strings.Contains(name, "/") {
		abs, e = p.LookPath(name, pathList, t.Cwd)
	} else {
		abs = resolve(t.Cwd, name)
	}
	if e != 0 {
		t.Close(p)
		return errnoRet(e)
	}
	if len(argv) == 0 {
		argv = []string{name}
	}
	if inherit && t.Cwd != cwd {
		// An inherited environment follows the CHDIR ops: $PWD is where a
		// wasip1 guest takes its working directory from.
		kept := env[:0]
		for _, kv := range env {
			if !strings.HasPrefix(kv, "PWD=") {
				kept = append(kept, kv)
			}
		}
		env = append(kept, "PWD="+t.Cwd)
	}
	return p.SpawnWait(SpawnSpec{Path: abs, Argv: argv, Env: env, FDs: t}, func(c *Child, e uint32, m Memory) Result {
		if e != 0 {
			return errnoRet(e)
		}
		if !writeU32(m, retPid, c.Pid()) {
			return errnoRet(EFAULT)
		}
		return errnoRet(0)
	})
}

// join implements proc_join(pid *option_pid, flags, status *join_status).
func (w *wasixInst) join(pidPtr, flags, statusPtr uint32, m Memory) Result {
	opt, e := readBytes(m, pidPtr, 8)
	if e != 0 {
		return errnoRet(e)
	}
	var pid uint32
	switch opt[0] {
	case 0: // none: any child
	case 1:
		pid = binary.LittleEndian.Uint32(opt[4:])
		if pid == 0 {
			return errnoRet(EINVAL)
		}
	default:
		return errnoRet(EINVAL)
	}
	wake := w.p.ChildChanged() // before Reap, so an exit in between wakes us
	c, e := w.p.Reap(pid)
	var st [6]byte
	switch {
	case e == EAGAIN && flags&joinNonBlocking != 0:
		st[0] = joinNothing
		if !m.Write(statusPtr, st[:]) {
			return errnoRet(EFAULT)
		}
		return errnoRet(0)
	case e == EAGAIN:
		return Result{Wait: wake, Resume: func(m Memory) Result { return w.join(pidPtr, flags, statusPtr, m) }}
	case e != 0:
		return errnoRet(e)
	}
	s := c.Status()
	if s.Signal != 0 {
		st[0] = joinExitSignal
		binary.LittleEndian.PutUint16(st[2:], 0)
		st[4] = byte(s.Signal)
	} else {
		// The code travels raw (its low 16 bits), not through an errno.
		st[0] = joinExitNormal
		binary.LittleEndian.PutUint16(st[2:], uint16(s.Code))
	}
	var o [8]byte
	o[0] = 1
	binary.LittleEndian.PutUint32(o[4:], c.Pid())
	if !m.Write(pidPtr, o[:]) || !m.Write(statusPtr, st[:]) {
		return errnoRet(EFAULT)
	}
	return errnoRet(0)
}

// parent implements proc_parent: the parent of any live process of the
// container (0 for its first process).
func (w *wasixInst) parent(pid, ret uint32, m Memory) Result {
	var ppid uint32
	switch {
	case pid == w.p.Pid():
		if pp := w.p.Parent(); pp != nil {
			ppid = pp.Pid()
		}
	case w.p.Group() != nil:
		pp, ok := w.p.Group().Pids()[pid]
		if !ok {
			return errnoRet(ESRCH)
		}
		ppid = pp
	default:
		return errnoRet(ESRCH)
	}
	if !writeU32(m, ret, ppid) {
		return errnoRet(EFAULT)
	}
	return errnoRet(0)
}

// tty is __wasi_tty_t: cols, rows, width, height (u32), then stdin_tty,
// stdout_tty, stderr_tty, echo, line_buffered, line_feeds (bools); 24
// bytes.
func (w *wasixInst) ttyGet(ptr uint32, m Memory) Result {
	var b [24]byte
	if t := w.p.Terminal(); t != nil {
		cols, rows := t.Size()
		binary.LittleEndian.PutUint32(b[0:], uint32(cols))
		binary.LittleEndian.PutUint32(b[4:], uint32(rows))
		echo, line := t.Mode()
		b[19], b[20] = boolByte(echo), boolByte(line)
	}
	for fd := uint32(0); fd < 3; fd++ {
		b[16+fd] = boolByte(w.p.IsTerminal(fd))
	}
	if !m.Write(ptr, b[:]) {
		return errnoRet(EFAULT)
	}
	return errnoRet(0)
}

// ttySet changes echo and line mode; the window size belongs to the front
// end and is not changed from the guest.
func (w *wasixInst) ttySet(ptr uint32, m Memory) Result {
	b, e := readBytes(m, ptr, 24)
	if e != 0 {
		return errnoRet(e)
	}
	t := w.p.Terminal()
	if t == nil {
		return errnoRet(ENOTTY)
	}
	t.SetMode(b[19] != 0, b[20] != 0)
	return errnoRet(0)
}

func boolByte(v bool) byte {
	if v {
		return 1
	}
	return 0
}
