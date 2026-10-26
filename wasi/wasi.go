// Package wasi is a WASI preview 1 (wasi_snapshot_preview1) host written
// against a byte-addressed guest memory and the engine's rootfs.FS.
//
// It implements every function Go's wasip1 port imports (runtime,
// syscall, internal/syscall/unix), with the layouts and errno values of
// the snapshot-01 specification (docs/REFERENCES.md). It knows nothing of
// a WebAssembly runtime: a host calls Proc.Call with the raw arguments and
// a Memory, so the same code serves the native executor (as a wazero host
// module) and the browser executor (from a worker, over shared memory).
package wasi

import (
	"crypto/rand"
	"encoding/binary"
	"io"
	"io/fs"
	"math"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
)

// Memory is the guest's linear memory. Read returns a slice valid until
// the next call on the Memory; both return false out of bounds.
type Memory interface {
	Read(off, n uint32) ([]byte, bool)
	Write(off uint32, b []byte) bool
}

// Errno values of wasi_snapshot_preview1 (enum order of the spec).
const (
	ESUCCESS     = 0
	E2BIG        = 1
	EACCES       = 2
	EAGAIN       = 6
	EBADF        = 8
	EEXIST       = 20
	EFAULT       = 21
	EINVAL       = 28
	EIO          = 29
	EISDIR       = 31
	ELOOP        = 32
	ENAMETOOLONG = 37
	ENOENT       = 44
	ENOSPC       = 51
	ENOSYS       = 52
	ENOTDIR      = 54
	ENOTEMPTY    = 55
	ENOTSUP      = 58
	EPERM        = 63
	EROFS        = 69
	ESPIPE       = 70
	ENOTCAPABLE  = 76
)

// File types.
const (
	ftUnknown   = 0
	ftCharDev   = 2
	ftDirectory = 3
	ftRegular   = 4
	ftSymlink   = 7
)

// Flags.
const (
	lookupSymlinkFollow = 1

	oflagCreat     = 1
	oflagDirectory = 2
	oflagExcl      = 4
	oflagTrunc     = 8

	fdflagAppend = 1

	rightFdRead  = 1 << 1
	rightFdWrite = 1 << 6
	allRights    = uint64(math.MaxUint32)

	fstAtim    = 1
	fstAtimNow = 2
	fstMtim    = 4
	fstMtimNow = 8

	whenceSet = 0
	whenceCur = 1
	whenceEnd = 2

	eventClock   = 0
	eventFdRead  = 1
	eventFdWrite = 2

	subclockAbstime = 1
)

// Param types of a function's core-wasm signature.
const (
	I32 = 'i'
	I64 = 'I'
)

// Func describes one import.
type Func struct {
	Name   string
	Params string // one of I32/I64 per parameter
	// NoResult is set for proc_exit; every other function returns an errno.
	NoResult bool
}

// Funcs lists the imports, in a fixed order a host may index by.
var Funcs = []Func{
	{"args_get", "ii", false},
	{"args_sizes_get", "ii", false},
	{"environ_get", "ii", false},
	{"environ_sizes_get", "ii", false},
	{"clock_res_get", "ii", false},
	{"clock_time_get", "iIi", false},
	{"fd_advise", "iIIi", false},
	{"fd_allocate", "iII", false},
	{"fd_close", "i", false},
	{"fd_datasync", "i", false},
	{"fd_fdstat_get", "ii", false},
	{"fd_fdstat_set_flags", "ii", false},
	{"fd_fdstat_set_rights", "iII", false},
	{"fd_filestat_get", "ii", false},
	{"fd_filestat_set_size", "iI", false},
	{"fd_filestat_set_times", "iIIi", false},
	{"fd_pread", "iiiIi", false},
	{"fd_prestat_get", "ii", false},
	{"fd_prestat_dir_name", "iii", false},
	{"fd_pwrite", "iiiIi", false},
	{"fd_read", "iiii", false},
	{"fd_readdir", "iiiIi", false},
	{"fd_renumber", "ii", false},
	{"fd_seek", "iIii", false},
	{"fd_sync", "i", false},
	{"fd_tell", "ii", false},
	{"fd_write", "iiii", false},
	{"path_create_directory", "iii", false},
	{"path_filestat_get", "iiiii", false},
	{"path_filestat_set_times", "iiiiIIi", false},
	{"path_link", "iiiiiii", false},
	{"path_open", "iiiiiIIii", false},
	{"path_readlink", "iiiiii", false},
	{"path_remove_directory", "iii", false},
	{"path_rename", "iiiiii", false},
	{"path_symlink", "iiiii", false},
	{"path_unlink_file", "iii", false},
	{"poll_oneoff", "iiii", false},
	{"proc_exit", "i", true},
	{"proc_raise", "i", false},
	{"sched_yield", "", false},
	{"random_get", "ii", false},
	{"sock_accept", "iii", false},
	{"sock_recv", "iiiiii", false},
	{"sock_send", "iiiii", false},
	{"sock_shutdown", "ii", false},
}

// FuncIndex maps a name to its index in Funcs.
var FuncIndex = func() map[string]int {
	m := make(map[string]int, len(Funcs))
	for i, f := range Funcs {
		m[f.Name] = i
	}
	return m
}()

// Config is a process's world.
type Config struct {
	Args, Env      []string
	FS             *rootfs.FS
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Now is the realtime clock (default time.Now).
	Now func() time.Time
	// Rand is the randomness source (default crypto/rand).
	Rand io.Reader
}

type fdKind int

const (
	kindStdin fdKind = iota
	kindStdout
	kindStderr
	kindPreopen
	kindFile
	kindDir
)

type fdEntry struct {
	kind   fdKind
	file   *rootfs.File
	path   string // canonical path for directories (and files, for info)
	append bool
	write  bool
	read   bool
	dirent []direntry // readdir snapshot, taken at cookie 0
}

type direntry struct {
	name string
	ino  uint64
	typ  uint8
}

// Proc is one guest process's WASI state.
type Proc struct {
	cfg   Config
	fds   map[uint32]*fdEntry
	next  uint32
	start time.Time
}

// MaxFDs bounds the descriptors a process may hold.
const MaxFDs = 1024

// New returns a process with stdin, stdout, stderr and "/" preopened as 3.
func New(cfg Config) *Proc {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Rand == nil {
		cfg.Rand = rand.Reader
	}
	if cfg.Stdout == nil {
		cfg.Stdout = io.Discard
	}
	if cfg.Stderr == nil {
		cfg.Stderr = io.Discard
	}
	p := &Proc{cfg: cfg, fds: map[uint32]*fdEntry{}, next: 4, start: time.Now()}
	p.fds[0] = &fdEntry{kind: kindStdin, read: true}
	p.fds[1] = &fdEntry{kind: kindStdout, write: true}
	p.fds[2] = &fdEntry{kind: kindStderr, write: true}
	p.fds[3] = &fdEntry{kind: kindPreopen, path: "/", read: true}
	return p
}

// Result is what a call returned.
type Result struct {
	Errno uint32
	// Sleep asks the host to block the guest this long before returning
	// (poll_oneoff on clocks). The host may cut it short on a kill.
	Sleep time.Duration
	// Exit is set by proc_exit, with Code.
	Exit bool
	Code uint32
}

func ok() Result               { return Result{} }
func fail(e uint32) Result     { return Result{Errno: e} }
func le32(b []byte) uint32     { return binary.LittleEndian.Uint32(b) }
func le64(b []byte) uint64     { return binary.LittleEndian.Uint64(b) }
func put32(b []byte, v uint32) { binary.LittleEndian.PutUint32(b, v) }
func put64(b []byte, v uint64) { binary.LittleEndian.PutUint64(b, v) }

func writeU32(m Memory, off, v uint32) bool {
	var b [4]byte
	put32(b[:], v)
	return m.Write(off, b[:])
}

func writeU64(m Memory, off uint32, v uint64) bool {
	var b [8]byte
	put64(b[:], v)
	return m.Write(off, b[:])
}

func readU32(m Memory, off uint32) (uint32, bool) {
	b, ok := m.Read(off, 4)
	if !ok {
		return 0, false
	}
	return le32(b), true
}

// Call runs import number fn of Funcs with args (one per parameter, i32
// values in the low 32 bits).
func (p *Proc) Call(fn int, args []uint64, m Memory) Result {
	if fn < 0 || fn >= len(Funcs) || len(args) != len(Funcs[fn].Params) {
		return fail(ENOSYS)
	}
	a := func(i int) uint32 { return uint32(args[i]) }
	switch Funcs[fn].Name {
	case "args_get":
		return p.strings(p.cfg.Args, a(0), a(1), m)
	case "args_sizes_get":
		return p.sizes(p.cfg.Args, a(0), a(1), m)
	case "environ_get":
		return p.strings(p.cfg.Env, a(0), a(1), m)
	case "environ_sizes_get":
		return p.sizes(p.cfg.Env, a(0), a(1), m)
	case "clock_res_get":
		if !writeU64(m, a(1), 1000) {
			return fail(EFAULT)
		}
		return ok()
	case "clock_time_get":
		t, e := p.clock(a(0))
		if e != 0 {
			return fail(e)
		}
		if !writeU64(m, a(2), t) {
			return fail(EFAULT)
		}
		return ok()
	case "fd_advise", "fd_allocate", "fd_datasync", "fd_sync", "fd_fdstat_set_rights":
		if p.fds[a(0)] == nil {
			return fail(EBADF)
		}
		return ok()
	case "fd_close":
		return p.fdClose(a(0))
	case "fd_fdstat_get":
		return p.fdstat(a(0), a(1), m)
	case "fd_fdstat_set_flags":
		f := p.fds[a(0)]
		if f == nil {
			return fail(EBADF)
		}
		f.append = a(1)&fdflagAppend != 0
		if f.file != nil {
			f.file.SetAppend(f.append)
		}
		return ok()
	case "fd_filestat_get":
		return p.fdFilestat(a(0), a(1), m)
	case "fd_filestat_set_size":
		f, e := p.file(a(0))
		if e != 0 {
			return fail(e)
		}
		if args[1] > math.MaxInt64 {
			return fail(EINVAL)
		}
		return fail(errno(f.file.Truncate(int64(args[1]))))
	case "fd_filestat_set_times":
		f := p.fds[a(0)]
		if f == nil {
			return fail(EBADF)
		}
		if f.file == nil {
			return ok()
		}
		if t, set := p.times(args[2], a(3)); set {
			return fail(errno(f.file.Chtimes(t)))
		}
		return ok()
	case "fd_pread":
		return p.rw(a(0), a(1), a(2), int64(args[3]), true, false, a(4), m)
	case "fd_pwrite":
		return p.rw(a(0), a(1), a(2), int64(args[3]), true, true, a(4), m)
	case "fd_read":
		return p.rw(a(0), a(1), a(2), 0, false, false, a(3), m)
	case "fd_write":
		return p.rw(a(0), a(1), a(2), 0, false, true, a(3), m)
	case "fd_prestat_get":
		if f := p.fds[a(0)]; f == nil || f.kind != kindPreopen {
			return fail(EBADF)
		}
		var b [8]byte
		put32(b[4:], 1) // tag 0 (dir), name length 1: "/"
		if !m.Write(a(1), b[:]) {
			return fail(EFAULT)
		}
		return ok()
	case "fd_prestat_dir_name":
		if f := p.fds[a(0)]; f == nil || f.kind != kindPreopen {
			return fail(EBADF)
		}
		if a(2) < 1 {
			return fail(EINVAL)
		}
		if !m.Write(a(1), []byte("/")) {
			return fail(EFAULT)
		}
		return ok()
	case "fd_readdir":
		return p.readdir(a(0), a(1), a(2), args[3], a(4), m)
	case "fd_renumber":
		from, to := a(0), a(1)
		f := p.fds[from]
		if f == nil || p.fds[to] == nil {
			return fail(EBADF)
		}
		if old := p.fds[to]; old.file != nil {
			old.file.Close()
		}
		p.fds[to] = f
		delete(p.fds, from)
		return ok()
	case "fd_seek":
		return p.seek(a(0), int64(args[1]), a(2), a(3), m)
	case "fd_tell":
		return p.seek(a(0), 0, whenceCur, a(1), m)
	case "path_create_directory":
		path, e := p.path(a(0), a(1), a(2), m)
		if e != 0 {
			return fail(e)
		}
		return fail(errno(p.cfg.FS.Mkdir(path, 0o755)))
	case "path_filestat_get":
		path, e := p.path(a(0), a(2), a(3), m)
		if e != 0 {
			return fail(e)
		}
		var in *rootfs.Info
		var err error
		if a(1)&lookupSymlinkFollow != 0 {
			in, err = p.cfg.FS.Stat(path)
		} else {
			in, err = p.cfg.FS.Lstat(path)
		}
		if err != nil {
			return fail(errno(err))
		}
		return p.writeFilestat(in, a(4), m)
	case "path_filestat_set_times":
		path, e := p.path(a(0), a(2), a(3), m)
		if e != 0 {
			return fail(e)
		}
		t, set := p.times(args[5], a(6))
		if !set {
			if _, err := p.cfg.FS.Lstat(path); err != nil {
				return fail(errno(err))
			}
			return ok()
		}
		return fail(errno(p.cfg.FS.Chtimes(path, t, a(1)&lookupSymlinkFollow != 0)))
	case "path_link":
		oldp, e := p.path(a(0), a(2), a(3), m)
		if e != 0 {
			return fail(e)
		}
		newp, e := p.path(a(4), a(5), a(6), m)
		if e != 0 {
			return fail(e)
		}
		return fail(errno(p.cfg.FS.Link(oldp, newp)))
	case "path_open":
		return p.open(a(0), a(1), a(2), a(3), a(4), args[5], a(7), a(8), m)
	case "path_readlink":
		path, e := p.path(a(0), a(1), a(2), m)
		if e != 0 {
			return fail(e)
		}
		t, err := p.cfg.FS.Readlink(path)
		if err != nil {
			return fail(errno(err))
		}
		n := uint32(len(t))
		if n > a(4) {
			n = a(4)
		}
		if !m.Write(a(3), []byte(t[:n])) || !writeU32(m, a(5), n) {
			return fail(EFAULT)
		}
		return ok()
	case "path_remove_directory":
		path, e := p.path(a(0), a(1), a(2), m)
		if e != 0 {
			return fail(e)
		}
		return fail(errno(p.cfg.FS.Rmdir(path)))
	case "path_rename":
		oldp, e := p.path(a(0), a(1), a(2), m)
		if e != 0 {
			return fail(e)
		}
		newp, e := p.path(a(3), a(4), a(5), m)
		if e != 0 {
			return fail(e)
		}
		return fail(errno(p.cfg.FS.Rename(oldp, newp)))
	case "path_symlink":
		tb, okb := m.Read(a(0), a(1))
		if !okb {
			return fail(EFAULT)
		}
		target := string(tb)
		newp, e := p.path(a(2), a(3), a(4), m)
		if e != 0 {
			return fail(e)
		}
		return fail(errno(p.cfg.FS.Symlink(target, newp)))
	case "path_unlink_file":
		path, e := p.path(a(0), a(1), a(2), m)
		if e != 0 {
			return fail(e)
		}
		return fail(errno(p.cfg.FS.Remove(path)))
	case "poll_oneoff":
		return p.poll(a(0), a(1), a(2), a(3), m)
	case "proc_exit":
		return Result{Exit: true, Code: a(0)}
	case "sched_yield":
		return ok()
	case "random_get":
		// In chunks: the length is the guest's, and must not size an
		// allocation in the host.
		off, left := a(0), a(1)
		if uint64(off)+uint64(left) > math.MaxUint32+1 {
			return fail(EFAULT)
		}
		buf := make([]byte, min(left, 64<<10))
		for left > 0 {
			chunk := buf[:min(left, uint32(len(buf)))]
			if _, err := io.ReadFull(p.cfg.Rand, chunk); err != nil {
				return fail(EIO)
			}
			if !m.Write(off, chunk) {
				return fail(EFAULT)
			}
			off += uint32(len(chunk))
			left -= uint32(len(chunk))
		}
		return ok()
	case "proc_raise", "sock_recv", "sock_send":
		return fail(ENOSYS)
	case "sock_accept", "sock_shutdown":
		return fail(ENOTSUP)
	}
	return fail(ENOSYS)
}

func (p *Proc) strings(list []string, ptrs, buf uint32, m Memory) Result {
	for _, s := range list {
		if !writeU32(m, ptrs, buf) {
			return fail(EFAULT)
		}
		b := append([]byte(s), 0)
		if !m.Write(buf, b) {
			return fail(EFAULT)
		}
		ptrs += 4
		buf += uint32(len(b))
	}
	return ok()
}

func (p *Proc) sizes(list []string, countPtr, sizePtr uint32, m Memory) Result {
	n := 0
	for _, s := range list {
		n += len(s) + 1
	}
	if !writeU32(m, countPtr, uint32(len(list))) || !writeU32(m, sizePtr, uint32(n)) {
		return fail(EFAULT)
	}
	return ok()
}

func (p *Proc) clock(id uint32) (uint64, uint32) {
	switch id {
	case 0: // realtime
		return uint64(p.cfg.Now().UnixNano()), 0
	case 1, 2, 3: // monotonic, process and thread CPU time
		return uint64(time.Since(p.start).Nanoseconds()), 0
	}
	return 0, EINVAL
}

func (p *Proc) times(mtim uint64, flags uint32) (time.Time, bool) {
	switch {
	case flags&fstMtimNow != 0:
		return p.cfg.Now(), true
	case flags&fstMtim != 0:
		return time.Unix(0, int64(mtim)), true
	}
	return time.Time{}, false
}

func (p *Proc) file(fd uint32) (*fdEntry, uint32) {
	f := p.fds[fd]
	switch {
	case f == nil:
		return nil, EBADF
	case f.kind == kindDir || f.kind == kindPreopen:
		return nil, EISDIR
	case f.file == nil:
		return nil, EBADF
	}
	return f, 0
}

func (p *Proc) fdClose(fd uint32) Result {
	f := p.fds[fd]
	if f == nil {
		return fail(EBADF)
	}
	if f.file != nil {
		f.file.Close()
	}
	delete(p.fds, fd)
	return ok()
}

func typeOf(in *rootfs.Info) uint8 {
	switch md := in.Mode(); {
	case md.IsDir():
		return ftDirectory
	case md.IsRegular():
		return ftRegular
	case md&fs.ModeSymlink != 0:
		return ftSymlink
	}
	return ftUnknown
}

func (p *Proc) fdstat(fd, ptr uint32, m Memory) Result {
	f := p.fds[fd]
	if f == nil {
		return fail(EBADF)
	}
	var b [24]byte
	switch f.kind {
	case kindStdin, kindStdout, kindStderr:
		b[0] = ftCharDev
	case kindPreopen, kindDir:
		b[0] = ftDirectory
	default:
		b[0] = ftRegular
	}
	if f.append {
		binary.LittleEndian.PutUint16(b[2:], fdflagAppend)
	}
	put64(b[8:], allRights)
	put64(b[16:], allRights)
	if !m.Write(ptr, b[:]) {
		return fail(EFAULT)
	}
	return ok()
}

func (p *Proc) writeFilestat(in *rootfs.Info, ptr uint32, m Memory) Result {
	var b [64]byte
	put64(b[0:], 1)
	put64(b[8:], in.Ino)
	b[16] = typeOf(in)
	put64(b[24:], in.Nlink)
	put64(b[32:], uint64(in.Size()))
	t := uint64(in.ModTime().UnixNano())
	put64(b[40:], t)
	put64(b[48:], t)
	put64(b[56:], t)
	if !m.Write(ptr, b[:]) {
		return fail(EFAULT)
	}
	return ok()
}

func (p *Proc) fdFilestat(fd, ptr uint32, m Memory) Result {
	f := p.fds[fd]
	if f == nil {
		return fail(EBADF)
	}
	switch f.kind {
	case kindStdin, kindStdout, kindStderr:
		var b [64]byte
		b[16] = ftCharDev
		put64(b[24:], 1)
		if !m.Write(ptr, b[:]) {
			return fail(EFAULT)
		}
		return ok()
	case kindPreopen:
		in, err := p.cfg.FS.Stat("/")
		if err != nil {
			return fail(errno(err))
		}
		return p.writeFilestat(in, ptr, m)
	}
	in, err := f.file.Info()
	if err != nil {
		return fail(errno(err))
	}
	return p.writeFilestat(in, ptr, m)
}

// maxIO bounds one read or write (a short count is legal; the guest
// loops), so a single call never makes the host allocate more.
const maxIO = 16 << 20

func (p *Proc) rw(fd, iovs, n uint32, off int64, positional, write bool, resultPtr uint32, m Memory) Result {
	f := p.fds[fd]
	if f == nil {
		return fail(EBADF)
	}
	if f.kind == kindDir || f.kind == kindPreopen {
		return fail(EISDIR)
	}
	if write && !f.write || !write && !f.read {
		return fail(EBADF)
	}
	if positional && (f.file == nil || off < 0) {
		if f.file == nil {
			return fail(ESPIPE)
		}
		return fail(EINVAL)
	}
	if n > 1024 {
		return fail(EINVAL)
	}
	vec, okv := m.Read(iovs, n*8)
	if !okv {
		return fail(EFAULT)
	}
	vec = append([]byte(nil), vec...)
	var total uint32
	for i := uint32(0); i < n; i++ {
		ptr, ln := le32(vec[i*8:]), le32(vec[i*8+4:])
		if uint64(total)+uint64(ln) > maxIO {
			ln = maxIO - total
		}
		if ln == 0 {
			continue
		}
		if write {
			b, okb := m.Read(ptr, ln)
			if !okb {
				return fail(EFAULT)
			}
			var k int
			var err error
			switch {
			case f.kind == kindStdout:
				k, err = p.cfg.Stdout.Write(b)
			case f.kind == kindStderr:
				k, err = p.cfg.Stderr.Write(b)
			case positional:
				k, err = f.file.WriteAt(b, off+int64(total))
			case f.kind == kindStdin:
				return fail(EBADF)
			default:
				k, err = f.file.Write(b)
			}
			total += uint32(k)
			if err != nil {
				if total > 0 {
					break
				}
				return fail(errno(err))
			}
		} else {
			buf := make([]byte, ln)
			var k int
			var err error
			switch {
			case f.kind == kindStdin:
				if p.cfg.Stdin == nil {
					k, err = 0, io.EOF
				} else {
					k, err = p.cfg.Stdin.Read(buf)
				}
			case positional:
				k, err = f.file.ReadAt(buf, off+int64(total))
			default:
				k, err = f.file.Read(buf)
			}
			if k > 0 && !m.Write(ptr, buf[:k]) {
				return fail(EFAULT)
			}
			total += uint32(k)
			if err == io.EOF || k < int(ln) {
				break
			}
			if err != nil {
				if total > 0 {
					break
				}
				return fail(errno(err))
			}
		}
		if total >= maxIO {
			break
		}
	}
	if !writeU32(m, resultPtr, total) {
		return fail(EFAULT)
	}
	return ok()
}

func (p *Proc) seek(fd uint32, off int64, whence, resultPtr uint32, m Memory) Result {
	f := p.fds[fd]
	if f == nil {
		return fail(EBADF)
	}
	if f.file == nil {
		return fail(ESPIPE)
	}
	if f.kind == kindDir {
		if off != 0 || whence != whenceSet {
			return fail(EINVAL)
		}
	}
	var w int
	switch whence {
	case whenceSet:
		w = io.SeekStart
	case whenceCur:
		w = io.SeekCurrent
	case whenceEnd:
		w = io.SeekEnd
	default:
		return fail(EINVAL)
	}
	pos, err := f.file.Seek(off, w)
	if err != nil {
		return fail(errno(err))
	}
	if !writeU64(m, resultPtr, uint64(pos)) {
		return fail(EFAULT)
	}
	return ok()
}

// path resolves a guest path relative to a directory descriptor.
func (p *Proc) path(dirfd, ptr, n uint32, m Memory) (string, uint32) {
	d := p.fds[dirfd]
	if d == nil {
		return "", EBADF
	}
	if d.kind != kindDir && d.kind != kindPreopen {
		return "", ENOTDIR
	}
	if n > 4096 {
		return "", ENAMETOOLONG
	}
	b, okb := m.Read(ptr, n)
	if !okb {
		return "", EFAULT
	}
	rel := string(b)
	if rel == "" {
		return "", ENOENT
	}
	for i := 0; i < len(rel); i++ {
		if rel[i] == 0 {
			return "", EINVAL
		}
	}
	if rel[0] == '/' {
		// WASI paths are relative to a capability; an absolute one names
		// nothing the guest holds.
		return "", ENOTCAPABLE
	}
	if d.path == "/" {
		return "/" + rel, 0
	}
	return d.path + "/" + rel, 0
}

func (p *Proc) open(dirfd, lookup, ptr, n, oflags uint32, rightsBase uint64, fdflags, resultPtr uint32, m Memory) Result {
	if len(p.fds) >= MaxFDs {
		return fail(33) // EMFILE
	}
	path, e := p.path(dirfd, ptr, n, m)
	if e != 0 {
		return fail(e)
	}
	write := rightsBase&rightFdWrite != 0
	var fl rootfs.Flag
	if write {
		fl = rootfs.O_RDWR
		if rightsBase&rightFdRead == 0 {
			fl = rootfs.O_WRONLY
		}
	}
	if oflags&oflagCreat != 0 {
		fl |= rootfs.O_CREATE
	}
	if oflags&oflagExcl != 0 {
		fl |= rootfs.O_EXCL
	}
	if oflags&oflagTrunc != 0 {
		fl |= rootfs.O_TRUNC
	}
	if oflags&oflagDirectory != 0 {
		fl |= rootfs.O_DIRECTORY
	}
	if fdflags&fdflagAppend != 0 {
		fl |= rootfs.O_APPEND
	}
	if lookup&lookupSymlinkFollow == 0 {
		fl |= rootfs.O_NOFOLLOW
	}
	h, err := p.cfg.FS.OpenFile(path, fl, 0o644)
	if err != nil {
		return fail(errno(err))
	}
	ent := &fdEntry{kind: kindFile, file: h, read: fl&rootfs.O_ACCMODE != rootfs.O_WRONLY, write: write, append: fdflags&fdflagAppend != 0}
	if h.IsDir() {
		ent.kind = kindDir
		ent.path = canonical(path)
	}
	fd := p.next
	for p.fds[fd] != nil {
		fd++
	}
	p.next = fd + 1
	if !writeU32(m, resultPtr, fd) {
		h.Close()
		return fail(EFAULT)
	}
	p.fds[fd] = ent
	return ok()
}

// canonical cleans a path for use as a directory descriptor's base. The
// tree resolves symlinks itself; this only removes "." and "//".
func canonical(p string) string {
	out := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		if p[i] == '/' && len(out) > 0 && out[len(out)-1] == '/' {
			continue
		}
		out = append(out, p[i])
	}
	s := string(out)
	for len(s) > 1 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func (p *Proc) readdir(fd, buf, bufLen uint32, cookie uint64, resultPtr uint32, m Memory) Result {
	f := p.fds[fd]
	if f == nil {
		return fail(EBADF)
	}
	if f.kind != kindDir && f.kind != kindPreopen {
		return fail(ENOTDIR)
	}
	if cookie == 0 || f.dirent == nil {
		ents, err := p.cfg.FS.ReadDir(f.path)
		if err != nil {
			return fail(errno(err))
		}
		self, _ := p.cfg.FS.Stat(f.path)
		var selfIno uint64
		if self != nil {
			selfIno = self.Ino
		}
		list := []direntry{{".", selfIno, ftDirectory}, {"..", 0, ftDirectory}}
		for _, e := range ents {
			in, _ := e.Info()
			t := uint8(ftUnknown)
			if ri, ok := in.(*rootfs.Info); ok {
				t = typeOf(ri)
			}
			list = append(list, direntry{e.Name(), e.Ino(), t})
		}
		f.dirent = list
	}
	if cookie > uint64(len(f.dirent)) {
		if !writeU32(m, resultPtr, 0) {
			return fail(EFAULT)
		}
		return ok()
	}
	out := make([]byte, 0, min(bufLen, 64<<10)) // bufLen is the guest's
	for i := cookie; i < uint64(len(f.dirent)) && uint32(len(out)) < bufLen; i++ {
		e := f.dirent[i]
		var h [24]byte
		put64(h[0:], i+1)
		put64(h[8:], e.ino)
		put32(h[16:], uint32(len(e.name)))
		h[20] = e.typ
		out = append(out, h[:]...)
		out = append(out, e.name...)
	}
	if uint32(len(out)) > bufLen {
		out = out[:bufLen] // the spec lets the last entry be truncated
	}
	if !m.Write(buf, out) || !writeU32(m, resultPtr, uint32(len(out))) {
		return fail(EFAULT)
	}
	return ok()
}

func (p *Proc) poll(in, out, nsubs, resultPtr uint32, m Memory) Result {
	if nsubs == 0 {
		return fail(EINVAL)
	}
	if nsubs > 1024 {
		return fail(EINVAL)
	}
	subs, okb := m.Read(in, nsubs*48)
	if !okb {
		return fail(EFAULT)
	}
	subs = append([]byte(nil), subs...)
	type ev struct {
		userdata uint64
		typ      uint8
		errno    uint16
		timeout  time.Duration
	}
	var ready, clocks []ev
	now := time.Since(p.start)
	for i := uint32(0); i < nsubs; i++ {
		s := subs[i*48:]
		e := ev{userdata: le64(s[0:]), typ: s[8]}
		switch e.typ {
		case eventClock:
			id := le32(s[16:])
			timeout := le64(s[24:])
			flags := binary.LittleEndian.Uint16(s[40:])
			var d time.Duration
			if timeout > math.MaxInt64 {
				timeout = math.MaxInt64
			}
			if flags&subclockAbstime != 0 {
				var nowc uint64
				if id == 0 {
					nowc = uint64(p.cfg.Now().UnixNano())
				} else {
					nowc = uint64(now.Nanoseconds())
				}
				if timeout > nowc {
					d = time.Duration(timeout - nowc)
				}
			} else {
				d = time.Duration(timeout)
			}
			if id > 3 {
				e.errno = EINVAL
				ready = append(ready, e)
				continue
			}
			e.timeout = d
			clocks = append(clocks, e)
		case eventFdRead, eventFdWrite:
			if p.fds[le32(s[16:])] == nil {
				e.errno = EBADF
			}
			ready = append(ready, e)
		default:
			e.errno = EINVAL
			ready = append(ready, e)
		}
	}
	var sleep time.Duration
	if len(ready) == 0 {
		min := clocks[0].timeout
		for _, c := range clocks {
			if c.timeout < min {
				min = c.timeout
			}
		}
		sleep = min
		for _, c := range clocks {
			if c.timeout <= min {
				ready = append(ready, c)
			}
		}
	}
	buf := make([]byte, 32*len(ready))
	for i, e := range ready {
		b := buf[i*32:]
		put64(b[0:], e.userdata)
		binary.LittleEndian.PutUint16(b[8:], e.errno)
		b[10] = e.typ
	}
	if !m.Write(out, buf) || !writeU32(m, resultPtr, uint32(len(ready))) {
		return fail(EFAULT)
	}
	return Result{Sleep: sleep}
}
