package wasi

import (
	"path"
	"strings"
	"sync/atomic"
)

// Descriptor tables across processes. A spawned child starts from a copy
// of its parent's table with fd operations applied (posix_spawn's file
// actions). Copies share the open file description (the rootfs handle and
// its offset, a pipe end); each descriptor keeps its own flags (nonblock,
// cloexec), which dup2 copies. A file description is closed when its last
// descriptor in any process is.
//
// Sockets are not inherited: a child's copy of the table has none (the
// network stack is per process).

// desc counts the descriptors sharing one open file.
type desc struct{ refs atomic.Int32 }

// share marks f's file as shared by one more descriptor.
func share(f *fdEntry) {
	if f.file == nil {
		return
	}
	if f.desc == nil {
		f.desc = &desc{}
		f.desc.refs.Store(1)
	}
	f.desc.refs.Add(1)
}

// release drops what descriptor f holds: its file (when it was the last
// descriptor on it), its pipe end, its socket.
func (p *Proc) release(f *fdEntry) {
	if f.file != nil && (f.desc == nil || f.desc.refs.Add(-1) == 0) {
		f.file.Close()
	}
	if f.pipe != nil {
		f.pipe.unref(f.pipeW)
	}
	if f.sock != nil {
		p.closeSock(f.sock)
	}
}

// alloc places f at the lowest free descriptor from p.next.
func (p *Proc) alloc(f *fdEntry) uint32 {
	fd := p.next
	for p.fds[fd] != nil {
		fd++
	}
	p.next = fd + 1
	p.fds[fd] = f
	return fd
}

// copyEntry duplicates a descriptor (dup, dup2, a child's table): same
// open file description, same flags.
func copyEntry(f *fdEntry) *fdEntry {
	c := *f
	share(f)
	c.desc = f.desc
	if f.pipe != nil {
		f.pipe.ref(f.pipeW)
	}
	c.dirent = nil
	return &c
}

// SetCloexec sets or clears fd's close-on-spawn flag.
func (p *Proc) SetCloexec(fd uint32, on bool) uint32 {
	f := p.fds[fd]
	if f == nil {
		return EBADF
	}
	f.cloexec = on
	return 0
}

// Cloexec reports fd's close-on-spawn flag.
func (p *Proc) Cloexec(fd uint32) (bool, uint32) {
	f := p.fds[fd]
	if f == nil {
		return false, EBADF
	}
	return f.cloexec, 0
}

// Dup copies fd to the lowest free descriptor at or above from (fd_dup,
// F_DUPFD); the copy's cloexec is clear.
func (p *Proc) Dup(fd, from uint32) (uint32, uint32) {
	f := p.fds[fd]
	if f == nil {
		return 0, EBADF
	}
	if len(p.fds) >= MaxFDs || from >= MaxFDs {
		return 0, EMFILE
	}
	to := from
	for p.fds[to] != nil {
		to++
	}
	c := copyEntry(f)
	c.cloexec = false
	p.fds[to] = c
	return to, 0
}

// Dup2 makes to a copy of fd (closing what was at to); onto a preopen it
// fails with EBADF. The copy keeps fd's nonblock and clears cloexec.
func (p *Proc) Dup2(fd, to uint32) uint32 {
	f := p.fds[fd]
	if f == nil || to >= MaxFDs {
		return EBADF
	}
	if fd == to {
		return 0
	}
	if old := p.fds[to]; old != nil {
		if old.kind == kindPreopen {
			return EBADF
		}
		p.release(old)
	} else if len(p.fds) >= MaxFDs {
		return EMFILE
	}
	c := copyEntry(f)
	c.cloexec = false
	p.fds[to] = c
	return 0
}

// FDOpKind is a file action applied to a child's table.
type FDOpKind int

// File actions, in posix_spawn's terms.
const (
	// FDClose closes FD; closing a preopen is ignored (the child keeps
	// its capabilities).
	FDClose FDOpKind = iota
	// FDDup2 makes To a copy of FD (clearing To's cloexec); onto a
	// preopen it fails with EBADF.
	FDDup2
	// FDOpen opens Path (absolute, or relative to the child's cwd) at FD
	// with OFlags, FDFlags and Write, like path_open.
	FDOpen
	// FDChdir sets the child's cwd to Path (resolved like FDOpen).
	FDChdir
	// FDFchdir sets the child's cwd to the directory open at FD.
	FDFchdir
)

// FDOp is one file action.
type FDOp struct {
	Kind    FDOpKind
	FD, To  uint32
	Path    string
	OFlags  uint32 // preview 1 oflags (creat, directory, excl, trunc)
	FDFlags uint32 // preview 1 fdflags (append, nonblock)
	Write   bool
}

// FDTable is a child's initial descriptor table and cwd, made by
// CloneFDs and handed to Spawner.Spawn.
type FDTable struct {
	fds  map[uint32]*fdEntry
	next uint32
	Cwd  string
}

// Close releases a table that was never handed to a process.
func (t *FDTable) Close(p *Proc) {
	for _, f := range t.fds {
		p.release(f)
	}
	t.fds = nil
}

// CloneFDs copies this process's table (sockets excluded), applies ops in
// order, then drops every cloexec descriptor. cwd is the starting
// directory (the parent's PWD). It fails with the errno of the first
// operation that fails, releasing the partial copy.
func (p *Proc) CloneFDs(cwd string, ops []FDOp) (*FDTable, uint32) {
	t := &FDTable{fds: map[uint32]*fdEntry{}, next: p.next, Cwd: path.Clean("/" + cwd)}
	for fd, f := range p.fds {
		if f.sock != nil {
			continue
		}
		t.fds[fd] = copyEntry(f)
	}
	for _, op := range ops {
		if e := p.applyOp(t, op); e != 0 {
			t.Close(p)
			return nil, e
		}
	}
	for fd, f := range t.fds {
		if f.cloexec {
			p.release(f)
			delete(t.fds, fd)
		}
	}
	return t, 0
}

func (p *Proc) applyOp(t *FDTable, op FDOp) uint32 {
	switch op.Kind {
	case FDClose:
		f := t.fds[op.FD]
		if f == nil {
			return EBADF
		}
		if f.kind == kindPreopen {
			return 0
		}
		p.release(f)
		delete(t.fds, op.FD)
	case FDDup2:
		f := t.fds[op.FD]
		if f == nil || op.To >= MaxFDs {
			return EBADF
		}
		if old := t.fds[op.To]; old != nil {
			if old.kind == kindPreopen {
				return EBADF
			}
			if op.To == op.FD {
				old.cloexec = false
				return 0
			}
			p.release(old)
		}
		c := copyEntry(f)
		c.cloexec = false
		t.fds[op.To] = c
	case FDOpen:
		if op.FD >= MaxFDs || len(t.fds) >= MaxFDs {
			return EMFILE
		}
		if old := t.fds[op.FD]; old != nil {
			if old.kind == kindPreopen {
				return EBADF
			}
			p.release(old)
			delete(t.fds, op.FD)
		}
		f, e := p.openAbs(t, resolve(t.Cwd, op.Path), op)
		if e != 0 {
			return e
		}
		t.fds[op.FD] = f
	case FDChdir:
		dir := resolve(t.Cwd, op.Path)
		pre, rel := p.preopenFor(t, dir)
		if pre == nil {
			return ENOENT
		}
		in, err := pre.fs.Stat(rel)
		if err != nil {
			return errno(err)
		}
		if !in.Mode().IsDir() {
			return ENOTDIR
		}
		t.Cwd = dir
	case FDFchdir:
		f := t.fds[op.FD]
		if f == nil {
			return EBADF
		}
		if f.kind != kindDir && f.kind != kindPreopen {
			return ENOTDIR
		}
		t.Cwd = f.guestPath()
	default:
		return EINVAL
	}
	return 0
}

// guestPath is the absolute guest path of a directory descriptor.
func (f *fdEntry) guestPath() string {
	if f.kind == kindPreopen {
		return f.name
	}
	// f.path is in the tree of the preopen it was opened under, whose
	// guest path is f.root.
	return path.Clean(strings.TrimSuffix(f.root, "/") + "/" + f.path)
}

// resolve makes p absolute against cwd.
func resolve(cwd, p string) string {
	if strings.HasPrefix(p, "/") {
		return path.Clean(p)
	}
	return path.Clean(cwd + "/" + p)
}

// preopenFor finds the preopen of table t whose guest path is the longest
// prefix of abs, and abs relative to its tree (like wasi-libc's resolution
// of an absolute path).
func (p *Proc) preopenFor(t *FDTable, abs string) (*fdEntry, string) {
	var best *fdEntry
	bestLen := -1
	for _, f := range t.fds {
		if f.kind != kindPreopen {
			continue
		}
		n := f.name
		if n == "/" || abs == n || strings.HasPrefix(abs, strings.TrimSuffix(n, "/")+"/") {
			if len(n) > bestLen {
				best, bestLen = f, len(n)
			}
		}
	}
	if best == nil {
		return nil, ""
	}
	rel := strings.TrimPrefix(abs, strings.TrimSuffix(best.name, "/"))
	if rel == "" {
		rel = "/"
	}
	return best, rel
}

// ReadModule reads the module a child is to run: an absolute guest path,
// resolved through this process's preopens. ENOENT when nothing is
// there, ENOEXEC when it is not a regular file holding a WebAssembly
// module.
func (p *Proc) ReadModule(abs string) ([]byte, uint32) {
	in, e := p.Stat(abs, false)
	if e != 0 {
		return nil, e
	}
	if !in.Mode().IsRegular() {
		return nil, ENOEXEC
	}
	pre, rel := p.preopenFor(&FDTable{fds: p.fds}, resolve("/", abs))
	b, err := pre.fs.ReadFile(rel)
	if err != nil {
		return nil, errno(err)
	}
	if len(b) < 8 || string(b[:4]) != "\x00asm" {
		return nil, ENOEXEC
	}
	return b, 0
}

// openAbs opens an absolute guest path for a child's table.
func (p *Proc) openAbs(t *FDTable, abs string, op FDOp) (*fdEntry, uint32) {
	pre, rel := p.preopenFor(t, abs)
	if pre == nil {
		return nil, ENOENT
	}
	f, e := openEntry(pre, rel, lookupSymlinkFollow, op.OFlags, op.Write, true, op.FDFlags)
	return f, e
}
