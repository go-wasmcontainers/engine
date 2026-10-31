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
	"net/netip"
	"sync"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver"
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
	EXDEV        = 75
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
	// The WasmEdge socket extension (V2), in wasi_snapshot_preview1.
	{"sock_open", "iii", false},
	{"sock_bind", "iii", false},
	{"sock_listen", "ii", false},
	{"sock_connect", "iii", false},
	{"sock_recv_from", "iiiiiiii", false},
	{"sock_send_to", "iiiiiii", false},
	{"sock_getsockopt", "iiiii", false},
	{"sock_setsockopt", "iiiii", false},
	{"sock_getlocaladdr", "iii", false},
	{"sock_getpeeraddr", "iii", false},
	{"sock_getaddrinfo", "iiiiiiii", false},
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
	Args, Env []string
	FS        *rootfs.FS
	// Mounts are further trees, each preopened under its own path after
	// "/" (fd 4 onwards), as wasmtime's --dir host::guest does: a guest
	// resolves an absolute path against the longest matching preopen.
	Mounts         []Mount
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	// Now is the realtime clock (default time.Now).
	Now func() time.Time
	// Rand is the randomness source (default crypto/rand).
	Rand io.Reader
	// Net is the container's place on a network; nil means no network
	// (sock_open fails with ENOTSUP).
	Net netdriver.Endpoint
	// Modules are the host modules served beside wasi_snapshot_preview1,
	// in the order of the import table (Imports); an executor passes the
	// same list it defined the imports from.
	Modules []HostModule
	// Caps are the capabilities the process was started with ("gpu").
	Caps []string
	// Parent is the process that started this one, if any.
	Parent *Proc
	// FDs, when set, is the initial descriptor table (a spawned child's,
	// from CloneFDs) instead of stdio, "/" and the mounts.
	FDs *FDTable
	// Group is the container's process tree (pids, limits, kill); nil
	// makes a group of this process alone.
	Group *Group
	// Spawner starts child processes (the executor's); nil refuses them.
	Spawner Spawner
	// Terminal describes the container's terminal, if any.
	Terminal *Terminal
	// MemoryLimitBytes is this process's linear-memory limit, reserved
	// in the group's memory budget.
	MemoryLimitBytes int64
	// Admitted says Group.Admit already reserved this process's room (a
	// Spawner sets it).
	Admitted bool
	// Label names the process's container (runtime.Spec.ID): host modules
	// report it with what they show of the process (a surface's pane).
	Label string
}

// Label is the process's container label (Config.Label).
func (p *Proc) Label() string { return p.cfg.Label }

type fdKind int

const (
	kindStdin fdKind = iota
	kindStdout
	kindStderr
	kindPreopen
	kindFile
	kindDir
	kindSocket
	kindPipe
)

// Mount is a tree preopened at Path; ReadOnly refuses every change made
// through it with EROFS.
type Mount struct {
	Path     string
	FS       *rootfs.FS
	ReadOnly bool
}

type fdEntry struct {
	kind fdKind
	// fs is the tree this descriptor belongs to, ro whether it was reached
	// through a read-only mount; name is a preopen's guest path.
	fs     *rootfs.FS
	ro     bool
	name   string
	file   *rootfs.File
	path   string // canonical path for directories (and files, for info)
	append bool
	write  bool
	read   bool
	dirent []direntry // readdir snapshot, taken at cookie 0
	sock   *sock
	// root is the guest path of the preopen a file or directory was
	// opened under (path is relative to its tree).
	root string
	// desc is shared by the copies of this descriptor (dup2, a child's
	// table); nil while it is the only one.
	desc *desc
	// pipe is a pipe end (kindPipe), its write end when pipeW.
	pipe  *pipe
	pipeW bool
	// nonblock and cloexec are per-descriptor flags (fd_fdstat_set_flags,
	// the WASIX fd flags); a dup2 copies nonblock and clears cloexec.
	nonblock, cloexec bool
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
	net   *netState
	host  *hostState
	pid   uint32

	childMu   sync.Mutex
	children  []*Child
	childWake chan struct{} // closed and replaced at each child exit
	closed    bool
	// stdin is cfg.Stdin, wrapped (nil: an empty stdin).
	stdin *Stdin
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
	// Stdin never blocks a host call (stdin.go). A host that spawns
	// children hands them the same *Stdin (NewStdin returns it as is).
	var stdin *Stdin
	if cfg.Stdin != nil {
		stdin = NewStdin(cfg.Stdin)
		cfg.Stdin = stdin
	}
	p := &Proc{cfg: cfg, stdin: stdin, fds: map[uint32]*fdEntry{}, next: 4, start: time.Now(), host: newHostState(cfg.Modules)}
	p.netInit()
	p.childWake = make(chan struct{})
	if cfg.FDs != nil {
		p.fds, p.next = cfg.FDs.fds, cfg.FDs.next
		cfg.FDs.fds = nil // the table now belongs to the process
	} else {
		p.fds[0] = &fdEntry{kind: kindStdin, read: true}
		p.fds[1] = &fdEntry{kind: kindStdout, write: true}
		p.fds[2] = &fdEntry{kind: kindStderr, write: true}
		p.fds[3] = &fdEntry{kind: kindPreopen, fs: cfg.FS, name: "/", path: "/", read: true}
		for _, mt := range cfg.Mounts {
			p.fds[p.next] = &fdEntry{kind: kindPreopen, fs: mt.FS, ro: mt.ReadOnly, name: mt.Path, path: "/", read: true}
			p.next++
		}
	}
	if p.cfg.Group == nil {
		p.cfg.Group = NewGroup(GroupLimits{})
	}
	for _, f := range p.fds {
		if f.kind == kindPreopen && f.root == "" {
			f.root = f.name
		}
	}
	p.pid = p.cfg.Group.register(p, cfg.Admitted)
	return p
}

// at resolves a guest path against a directory descriptor: the tree it
// lies in, the path in that tree, and whether it is read-only there.
func (p *Proc) at(dirfd, ptr, n uint32, m Memory) (*fdEntry, string, uint32) {
	path, e := p.path(dirfd, ptr, n, m)
	if e != 0 {
		return nil, "", e
	}
	return p.fds[dirfd], path, 0
}

// change is at for an operation that modifies the tree.
func (p *Proc) change(dirfd, ptr, n uint32, m Memory) (*fdEntry, string, uint32) {
	d, path, e := p.at(dirfd, ptr, n, m)
	if e == 0 && d.ro {
		return nil, "", EROFS
	}
	return d, path, e
}

// Result is what a call returned.
type Result struct {
	Errno uint32
	// Sleep asks the host to block the guest this long before returning
	// (poll_oneoff on clocks). The host may cut it short on a kill.
	Sleep time.Duration
	// Wait, when set, asks the host to block until Wait is closed, Sleep
	// (if not zero) has passed, or the process is killed, and then to
	// return what Resume returns instead (which may ask to wait again).
	// The call itself never blocks: sockets wait this way.
	Wait   <-chan struct{}
	Resume func(m Memory) Result
	// Exit is set by proc_exit, with Code.
	Exit bool
	Code uint32
	// Ret is a host-module function's result (HostFunc.Result); the
	// wasi_snapshot_preview1 functions return Errno instead.
	Ret uint64
	// Realloc, when set, asks the host to allocate in the guest first
	// (see Realloc); Wait and Realloc are never both set.
	Realloc *Realloc
	// Trap, when not empty, ends the guest with a trap of that message (a
	// host module's answer to a call the canonical ABI says must trap).
	Trap string
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
		if f.sock != nil {
			f.sock.nonblock = a(1)&fdflagNonblock != 0
			return ok()
		}
		f.nonblock = a(1)&fdflagNonblock != 0
		if f.pipe != nil {
			return ok()
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
			if f.ro {
				return fail(EROFS)
			}
			return fail(errno(f.file.Chtimes(t)))
		}
		return ok()
	case "fd_pread":
		return p.rw(a(0), a(1), a(2), int64(args[3]), true, false, a(4), m)
	case "fd_pwrite":
		return p.rw(a(0), a(1), a(2), int64(args[3]), true, true, a(4), m)
	case "fd_read":
		if f := p.fds[a(0)]; f != nil && f.sock != nil {
			return p.recv(a(0), a(1), a(2), 0, m, func(k uint32, _ bool, _ netip.AddrPort) Result {
				if !writeU32(m, a(3), k) {
					return fail(EFAULT)
				}
				return ok()
			})
		}
		return p.rw(a(0), a(1), a(2), 0, false, false, a(3), m)
	case "fd_write":
		if f := p.fds[a(0)]; f != nil && f.sock != nil {
			return p.send(a(0), a(1), a(2), netip.AddrPort{}, a(3), m)
		}
		return p.rw(a(0), a(1), a(2), 0, false, true, a(3), m)
	case "fd_prestat_get":
		f := p.fds[a(0)]
		if f == nil || f.kind != kindPreopen {
			return fail(EBADF)
		}
		var b [8]byte
		put32(b[4:], uint32(len(f.name))) // tag 0 (dir), then the name's length
		if !m.Write(a(1), b[:]) {
			return fail(EFAULT)
		}
		return ok()
	case "fd_prestat_dir_name":
		f := p.fds[a(0)]
		if f == nil || f.kind != kindPreopen {
			return fail(EBADF)
		}
		if a(2) < uint32(len(f.name)) {
			return fail(EINVAL)
		}
		if !m.Write(a(1), []byte(f.name)) {
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
		if from == to {
			return ok()
		}
		p.release(p.fds[to])
		p.fds[to] = f
		delete(p.fds, from)
		return ok()
	case "fd_seek":
		return p.seek(a(0), int64(args[1]), a(2), a(3), m)
	case "fd_tell":
		return p.seek(a(0), 0, whenceCur, a(1), m)
	case "path_create_directory":
		d, path, e := p.change(a(0), a(1), a(2), m)
		if e != 0 {
			return fail(e)
		}
		return fail(errno(d.fs.Mkdir(path, 0o755)))
	case "path_filestat_get":
		d, path, e := p.at(a(0), a(2), a(3), m)
		if e != 0 {
			return fail(e)
		}
		var in *rootfs.Info
		var err error
		if a(1)&lookupSymlinkFollow != 0 {
			in, err = d.fs.Stat(path)
		} else {
			in, err = d.fs.Lstat(path)
		}
		if err != nil {
			return fail(errno(err))
		}
		return p.writeFilestat(in, a(4), m)
	case "path_filestat_set_times":
		d, path, e := p.at(a(0), a(2), a(3), m)
		if e != 0 {
			return fail(e)
		}
		t, set := p.times(args[5], a(6))
		if !set {
			if _, err := d.fs.Lstat(path); err != nil {
				return fail(errno(err))
			}
			return ok()
		}
		if d.ro {
			return fail(EROFS)
		}
		return fail(errno(d.fs.Chtimes(path, t, a(1)&lookupSymlinkFollow != 0)))
	case "path_link":
		od, oldp, e := p.change(a(0), a(2), a(3), m)
		if e != 0 {
			return fail(e)
		}
		nd, newp, e := p.change(a(4), a(5), a(6), m)
		if e != 0 {
			return fail(e)
		}
		if od.fs != nd.fs {
			return fail(EXDEV)
		}
		return fail(errno(od.fs.Link(oldp, newp)))
	case "path_open":
		return p.open(a(0), a(1), a(2), a(3), a(4), args[5], a(7), a(8), m)
	case "path_readlink":
		d, path, e := p.at(a(0), a(1), a(2), m)
		if e != 0 {
			return fail(e)
		}
		t, err := d.fs.Readlink(path)
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
		d, path, e := p.change(a(0), a(1), a(2), m)
		if e != 0 {
			return fail(e)
		}
		return fail(errno(d.fs.Rmdir(path)))
	case "path_rename":
		od, oldp, e := p.change(a(0), a(1), a(2), m)
		if e != 0 {
			return fail(e)
		}
		nd, newp, e := p.change(a(3), a(4), a(5), m)
		if e != 0 {
			return fail(e)
		}
		if od.fs != nd.fs {
			return fail(EXDEV) // across mounts: the guest copies
		}
		return fail(errno(od.fs.Rename(oldp, newp)))
	case "path_symlink":
		tb, okb := m.Read(a(0), a(1))
		if !okb {
			return fail(EFAULT)
		}
		target := string(tb)
		d, newp, e := p.change(a(2), a(3), a(4), m)
		if e != 0 {
			return fail(e)
		}
		return fail(errno(d.fs.Symlink(target, newp)))
	case "path_unlink_file":
		d, path, e := p.change(a(0), a(1), a(2), m)
		if e != 0 {
			return fail(e)
		}
		return fail(errno(d.fs.Remove(path)))
	case "poll_oneoff":
		return p.poll(a(0), a(1), a(2), a(3), m)
	case "proc_exit":
		// The raw u32: a WASIX parent sees it as is (wasix_test "exit
		// codes travel raw"); the container's own status is bounded by
		// runtime.ExitCodeOf.
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
	case "proc_raise":
		return fail(ENOSYS)
	case "sock_accept":
		return p.sockAccept(a(0), a(1), a(2), m)
	case "sock_recv":
		// fd, ri_data, ri_data_len, ri_flags, *ro_datalen, *ro_flags
		return p.recv(a(0), a(1), a(2), a(3), m, func(k uint32, trunc bool, _ netip.AddrPort) Result {
			return p.recvDone(k, trunc, a(4), a(5), m)
		})
	case "sock_send":
		// fd, si_data, si_data_len, si_flags, *so_datalen
		return p.send(a(0), a(1), a(2), netip.AddrPort{}, a(4), m)
	case "sock_shutdown":
		return p.sockShutdown(a(0), a(1))
	case "sock_open":
		return p.sockOpen(a(0), a(1), a(2), m)
	case "sock_bind":
		return p.sockBind(a(0), a(1), a(2), m)
	case "sock_listen":
		return p.sockListen(a(0))
	case "sock_connect":
		return p.sockConnect(a(0), a(1), a(2), m)
	case "sock_recv_from":
		// fd, iovs, iovs_len, *address, ri_flags, *port, *nread, *ro_flags
		return p.recv(a(0), a(1), a(2), a(4), m, func(k uint32, trunc bool, from netip.AddrPort) Result {
			if from.IsValid() {
				if e := writeAddr(m, a(3), from.Addr()); e != 0 {
					return fail(e)
				}
				if !writeU32(m, a(5), uint32(from.Port())) {
					return fail(EFAULT)
				}
			}
			return p.recvDone(k, trunc, a(6), a(7), m)
		})
	case "sock_send_to":
		// fd, iovs, iovs_len, *address, port, si_flags, *nwritten
		addr, e := readAddr(m, a(3))
		if e != 0 {
			return fail(e)
		}
		if a(4) > 65535 {
			return fail(EINVAL)
		}
		return p.send(a(0), a(1), a(2), netip.AddrPortFrom(addr, uint16(a(4))), a(6), m)
	case "sock_getsockopt":
		return p.sockGetOpt(a(0), a(1), a(2), a(3), a(4), m)
	case "sock_setsockopt":
		return p.sockSetOpt(a(0), a(1), a(2), a(3), a(4), m)
	case "sock_getlocaladdr":
		return p.sockAddr(a(0), a(1), a(2), false, m)
	case "sock_getpeeraddr":
		return p.sockAddr(a(0), a(1), a(2), true, m)
	case "sock_getaddrinfo":
		return p.sockGetAddrInfo(a(0), a(1), a(2), a(3), a(4), a(5), a(6), a(7), m)
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
	p.release(f)
	delete(p.fds, fd)
	return ok()
}

// sockType is a socket's preview 1 file type.
func sockType(s *sock) uint8 {
	if s.stype == sockDgram {
		return ftSocketDgram
	}
	return ftSocketStream
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

// fileType is a descriptor's preview 1 file type. A pipe end is
// "unknown": preview 1 has no FIFO type.
func (p *Proc) fileType(f *fdEntry) uint8 {
	switch f.kind {
	case kindStdin, kindStdout, kindStderr:
		// wasmtime's preview 1 (crates/wasi/src/p1.rs, From<IsATTY> for
		// Filetype): a terminal is a character device, anything else (a
		// pipe, a file, /dev/null) is "unknown". wasi-libc's isatty() and
		// Go's terminal checks read exactly this.
		if p.isTTY(f) {
			return ftCharDev
		}
		return ftUnknown
	case kindPreopen, kindDir:
		return ftDirectory
	case kindSocket:
		return sockType(f.sock)
	case kindPipe:
		return ftUnknown
	}
	return ftRegular
}

func (p *Proc) fdstat(fd, ptr uint32, m Memory) Result {
	f := p.fds[fd]
	if f == nil {
		return fail(EBADF)
	}
	var b [24]byte
	b[0] = p.fileType(f)
	rights := allRights
	switch f.kind {
	case kindStdin:
		// As wasmtime p1.rs fd_fdstat_get: no FD_SEEK/FD_TELL, which
		// wasi-libc's isatty() (libc-bottom-half/sources/isatty.c) requires
		// of a terminal.
		rights = rightFdRead
	case kindStdout, kindStderr:
		rights = rightFdWrite
	}
	var flags uint16
	if f.append {
		flags |= fdflagAppend
	}
	if f.nonblock || f.sock != nil && f.sock.nonblock {
		flags |= fdflagNonblock
	}
	binary.LittleEndian.PutUint16(b[2:], flags)
	put64(b[8:], rights)
	put64(b[16:], rights)
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
	case kindStdin, kindStdout, kindStderr, kindSocket, kindPipe:
		var b [64]byte
		b[16] = p.fileType(f)
		put64(b[24:], 1)
		if !m.Write(ptr, b[:]) {
			return fail(EFAULT)
		}
		return ok()
	case kindPreopen:
		in, err := f.fs.Stat("/")
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
	if f.pipe != nil {
		if positional {
			return fail(ESPIPE)
		}
		return p.pipeRW(f, iovs, n, write, resultPtr, m)
	}
	if f.kind == kindStdin && !write {
		if positional {
			return fail(ESPIPE)
		}
		return p.stdinRead(f, iovs, n, resultPtr, m)
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
	d, path, e := p.at(dirfd, ptr, n, m)
	if e != 0 {
		return fail(e)
	}
	ent, e := openEntry(d, path, lookup, oflags, rightsBase&rightFdWrite != 0, rightsBase&rightFdRead != 0, fdflags)
	if e != 0 {
		return fail(e)
	}
	fd := p.next
	for p.fds[fd] != nil {
		fd++
	}
	p.next = fd + 1
	if !writeU32(m, resultPtr, fd) {
		ent.file.Close()
		return fail(EFAULT)
	}
	p.fds[fd] = ent
	return ok()
}

// openEntry opens path in directory descriptor d's tree, as path_open
// does, and returns the new descriptor (not yet placed in a table).
func openEntry(d *fdEntry, path string, lookup, oflags uint32, write, readRight bool, fdflags uint32) (*fdEntry, uint32) {
	if d.ro && (write || oflags&(oflagCreat|oflagTrunc) != 0) {
		return nil, EROFS
	}
	var fl rootfs.Flag
	if write {
		fl = rootfs.O_RDWR
		if !readRight {
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
	h, err := d.fs.OpenFile(path, fl, 0o644)
	if err != nil {
		return nil, errno(err)
	}
	ent := &fdEntry{kind: kindFile, fs: d.fs, ro: d.ro, file: h, read: fl&rootfs.O_ACCMODE != rootfs.O_WRONLY, write: write,
		append: fdflags&fdflagAppend != 0, nonblock: fdflags&fdflagNonblock != 0}
	ent.root = d.root
	if d.kind == kindPreopen {
		ent.root = d.name
	}
	if h.IsDir() {
		ent.kind = kindDir
		ent.path = canonical(path)
	}
	return ent, 0
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
		ents, err := f.fs.ReadDir(f.path)
		if err != nil {
			return fail(errno(err))
		}
		self, _ := f.fs.Stat(f.path)
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
	return p.pollUntil(in, out, nsubs, resultPtr, time.Time{}, m)
}

// pollUntil is poll_oneoff; deadline, when set, is when the earliest clock
// of a poll that is waiting on sockets fires (a resumed poll keeps it).
func (p *Proc) pollUntil(in, out, nsubs, resultPtr uint32, deadline time.Time, m Memory) Result {
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
		nbytes   uint64
		hangup   bool
	}
	var ready, clocks []ev
	sockets := false
	wake := p.net.current() // before reading readiness: a change after it wakes us
	var pipeWakes []<-chan struct{}
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
			f := p.fds[le32(s[16:])]
			switch {
			case f == nil:
				e.errno = EBADF
			case f.sock != nil:
				ok, hup, nb := p.sockReady(f.sock, e.typ == eventFdWrite)
				if !ok {
					sockets = true
					continue
				}
				e.hangup, e.nbytes = hup, nb
			case f.kind == kindStdin && e.typ == eventFdRead && p.stdin != nil:
				ok, hup, nb, wk := p.stdin.readiness()
				if !ok {
					sockets = true
					pipeWakes = append(pipeWakes, wk)
					continue
				}
				e.hangup, e.nbytes = hup, nb
			case f.pipe != nil:
				if (e.typ == eventFdWrite) != f.pipeW {
					e.errno = EBADF
					break
				}
				ok, hup, nb, wk := f.pipe.readiness(f.pipeW)
				if !ok {
					sockets = true
					pipeWakes = append(pipeWakes, wk)
					continue
				}
				e.hangup, e.nbytes = hup, nb
			}
			ready = append(ready, e)
		default:
			e.errno = EINVAL
			ready = append(ready, e)
		}
	}
	var sleep time.Duration
	if len(ready) == 0 && sockets {
		// Wait for a socket to change or the earliest clock, whichever
		// comes first, then poll again.
		if len(clocks) > 0 && deadline.IsZero() {
			d := clocks[0].timeout
			for _, c := range clocks {
				d = min(d, c.timeout)
			}
			deadline = time.Now().Add(d)
		}
		if deadline.IsZero() || time.Now().Before(deadline) {
			var left time.Duration
			if !deadline.IsZero() {
				left = max(time.Until(deadline), time.Nanosecond)
			}
			stop := func() {}
			if len(pipeWakes) > 0 {
				wake, stop = anyOf(append(pipeWakes, wake))
			}
			return Result{Wait: wake, Sleep: left, Resume: func(m Memory) Result {
				stop() // woken or timed out: the merged wait's goroutines end here
				return p.pollUntil(in, out, nsubs, resultPtr, deadline, m)
			}}
		}
		for i := range clocks {
			clocks[i].timeout = 0 // the deadline has passed: every clock fires
		}
	}
	if len(ready) > 0 {
		// A clock that has already expired is reported with the
		// descriptors that are ready, never dropped.
		for _, c := range clocks {
			if c.timeout <= 0 {
				ready = append(ready, c)
			}
		}
	}
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
		if e.typ != eventClock {
			put64(b[16:], e.nbytes)
			if e.hangup {
				binary.LittleEndian.PutUint16(b[24:], eventHangup)
			}
		}
	}
	if !m.Write(out, buf) || !writeU32(m, resultPtr, uint32(len(ready))) {
		return fail(EFAULT)
	}
	return Result{Sleep: sleep}
}
