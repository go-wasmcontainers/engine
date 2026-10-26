package rootfs

import (
	"io"
	"io/fs"
	"math"
	"time"
)

// Flag is an open flag. The access modes are exclusive; the rest combine.
type Flag int

const (
	O_RDONLY Flag = 0
	O_WRONLY Flag = 1
	O_RDWR   Flag = 2
	// O_ACCMODE masks the access mode.
	O_ACCMODE Flag = 3

	O_CREATE    Flag = 1 << 3
	O_EXCL      Flag = 1 << 4
	O_TRUNC     Flag = 1 << 5
	O_APPEND    Flag = 1 << 6
	O_DIRECTORY Flag = 1 << 7
	O_NOFOLLOW  Flag = 1 << 8
)

func (fl Flag) writable() bool { return fl&O_ACCMODE == O_WRONLY || fl&O_ACCMODE == O_RDWR }
func (fl Flag) readable() bool { return fl&O_ACCMODE == O_RDONLY || fl&O_ACCMODE == O_RDWR }

// File is an open file or directory.
type File struct {
	fs     *FS
	n      *node
	name   string
	flag   Flag
	off    int64
	dir    []DirEntry // directory snapshot, taken at the first ReadDir
	dirPos int
	closed bool
}

// IOFS returns a read-only io/fs view of the tree, with fs.FS's strict
// path rules (unrooted, slash-separated, no "." or ".." elements), so the
// tree can be handed to anything that reads an fs.FS, fstest included.
func (f *FS) IOFS() fs.FS { return ioFS{f} }

type ioFS struct{ f *FS }

func (x ioFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, pathErr("open", name, fs.ErrInvalid)
	}
	h, err := x.f.OpenFile(name, O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	return h, nil
}

// OpenFile opens p with the given flags; perm is used when creating.
func (f *FS) OpenFile(p string, flag Flag, perm fs.FileMode) (*File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	follow := flag&O_NOFOLLOW == 0
	r, err := f.resolveLocked(p, follow)
	if err != nil {
		return nil, pathErr("open", p, err)
	}
	n := r.node
	name := baseOf(r.comps)
	if n != nil && flag&O_CREATE != 0 && flag&O_EXCL != 0 {
		return nil, pathErr("open", p, fs.ErrExist)
	}
	if n != nil && n.isSymlink() {
		// Only reachable with O_NOFOLLOW: POSIX answers ELOOP.
		return nil, pathErr("open", p, ErrLoop)
	}
	if n != nil && n.isDir() {
		if flag.writable() || flag&O_TRUNC != 0 {
			return nil, pathErr("open", p, ErrIsDir)
		}
		return &File{fs: f, n: n, name: name, flag: flag}, nil
	}
	if flag&O_DIRECTORY != 0 {
		if n == nil {
			return nil, pathErr("open", p, fs.ErrNotExist)
		}
		return nil, pathErr("open", p, ErrNotDir)
	}
	if n == nil {
		if flag&O_CREATE == 0 {
			return nil, pathErr("open", p, fs.ErrNotExist)
		}
		if len(r.comps) == 0 {
			return nil, pathErr("open", p, fs.ErrInvalid)
		}
		dir, err := f.mutDirLocked(r.comps[:len(r.comps)-1])
		if err != nil {
			return nil, pathErr("open", p, err)
		}
		if err := f.chargeLocked(0, 1); err != nil {
			return nil, pathErr("open", p, err)
		}
		n = f.newNode(perm.Perm())
		n.data = []byte{}
		dir.kids[name] = n
		dir.mtime = time.Now()
	} else if flag.writable() || flag&O_TRUNC != 0 {
		dir, err := f.mutDirLocked(r.comps[:len(r.comps)-1])
		if err != nil {
			return nil, pathErr("open", p, err)
		}
		if n.owner != f.gen {
			// Copy-up charges the copied bytes to this tree.
			if err := f.chargeLocked(int64(len(n.data)), 0); err != nil {
				return nil, pathErr("open", p, err)
			}
		}
		n = f.mutChildLocked(dir, name)
		if flag&O_TRUNC != 0 {
			f.bytes -= int64(len(n.data))
			if f.bytes < 0 {
				f.bytes = 0
			}
			n.data = n.data[:0]
			n.mtime = time.Now()
		}
	}
	return &File{fs: f, n: n, name: name, flag: flag}, nil
}

// Name returns the base name the file was opened by.
func (h *File) Name() string { return h.name }

// IsDir reports whether the file is a directory.
func (h *File) IsDir() bool { return h.n.isDir() }

// IsAppend reports whether writes go to the end.
func (h *File) IsAppend() bool { return h.flag&O_APPEND != 0 }

// SetAppend toggles append mode.
func (h *File) SetAppend(on bool) {
	if on {
		h.flag |= O_APPEND
	} else {
		h.flag &^= O_APPEND
	}
}

// Stat implements fs.File.
func (h *File) Stat() (fs.FileInfo, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.closed {
		return nil, pathErr("stat", h.name, fs.ErrClosed)
	}
	return infoOf(h.name, h.n), nil
}

// Info is Stat with the concrete type.
func (h *File) Info() (*Info, error) {
	fi, err := h.Stat()
	if err != nil {
		return nil, err
	}
	return fi.(*Info), nil
}

func (h *File) check(op string, write bool) error {
	if h.closed {
		return pathErr(op, h.name, fs.ErrClosed)
	}
	if h.n.isDir() {
		return pathErr(op, h.name, ErrIsDir)
	}
	if write && !h.flag.writable() {
		return pathErr(op, h.name, ErrBadFile)
	}
	if !write && !h.flag.readable() {
		return pathErr(op, h.name, ErrBadFile)
	}
	return nil
}

// Read implements io.Reader.
func (h *File) Read(p []byte) (int, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("read", false); err != nil {
		return 0, err
	}
	if h.off >= int64(len(h.n.data)) {
		return 0, io.EOF
	}
	n := copy(p, h.n.data[h.off:])
	h.off += int64(n)
	return n, nil
}

// ReadAt implements io.ReaderAt.
func (h *File) ReadAt(p []byte, off int64) (int, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("pread", false); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, pathErr("pread", h.name, fs.ErrInvalid)
	}
	if off >= int64(len(h.n.data)) {
		return 0, io.EOF
	}
	n := copy(p, h.n.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Write implements io.Writer.
func (h *File) Write(p []byte) (int, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("write", true); err != nil {
		return 0, err
	}
	off := h.off
	if h.flag&O_APPEND != 0 {
		off = int64(len(h.n.data))
	}
	n, err := h.writeAtLocked(p, off)
	h.off = off + int64(n)
	return n, err
}

// WriteAt implements io.WriterAt. It ignores O_APPEND, as pwrite does on
// most systems wasm guests expect.
func (h *File) WriteAt(p []byte, off int64) (int, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("pwrite", true); err != nil {
		return 0, err
	}
	return h.writeAtLocked(p, off)
}

func (h *File) writeAtLocked(p []byte, off int64) (int, error) {
	if off < 0 || off > math.MaxInt64-int64(len(p)) {
		return 0, pathErr("write", h.name, fs.ErrInvalid)
	}
	h.fs.changes++
	end := off + int64(len(p))
	cur := int64(len(h.n.data))
	if end > cur {
		if err := h.fs.chargeLocked(end-cur, 0); err != nil {
			return 0, pathErr("write", h.name, err)
		}
		if end > math.MaxInt {
			return 0, pathErr("write", h.name, ErrNoSpace)
		}
		h.n.data = grow(h.n.data, int(end))
	}
	copy(h.n.data[off:], p)
	h.n.mtime = time.Now()
	return len(p), nil
}

// grow extends b to n bytes, zero-filling, amortising reallocation.
func grow(b []byte, n int) []byte {
	if n <= cap(b) {
		old := len(b)
		b = b[:n]
		clear(b[old:])
		return b
	}
	c := 2 * cap(b)
	if c < n {
		c = n
	}
	if c-n > 64<<20 { // do not reserve more than 64 MiB of slack
		c = n + 64<<20
	}
	nb := make([]byte, n, c)
	copy(nb, b)
	return nb
}

// Seek implements io.Seeker. On a directory only (0, SeekStart) is
// accepted, which rewinds ReadDir.
func (h *File) Seek(offset int64, whence int) (int64, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.closed {
		return 0, pathErr("seek", h.name, fs.ErrClosed)
	}
	if h.n.isDir() {
		if offset != 0 || whence != io.SeekStart {
			return 0, pathErr("seek", h.name, fs.ErrInvalid)
		}
		h.dir, h.dirPos = nil, 0
		return 0, nil
	}
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = h.off + offset
	case io.SeekEnd:
		abs = int64(len(h.n.data)) + offset
	default:
		return 0, pathErr("seek", h.name, fs.ErrInvalid)
	}
	if abs < 0 {
		return 0, pathErr("seek", h.name, fs.ErrInvalid)
	}
	h.off = abs
	return abs, nil
}

// Truncate changes the file's size.
func (h *File) Truncate(size int64) error {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if err := h.check("truncate", true); err != nil {
		return err
	}
	if size < 0 {
		return pathErr("truncate", h.name, fs.ErrInvalid)
	}
	h.fs.changes++
	cur := int64(len(h.n.data))
	switch {
	case size > cur:
		if err := h.fs.chargeLocked(size-cur, 0); err != nil {
			return pathErr("truncate", h.name, err)
		}
		if size > math.MaxInt {
			return pathErr("truncate", h.name, ErrNoSpace)
		}
		h.n.data = grow(h.n.data, int(size))
	case size < cur:
		h.fs.bytes -= cur - size
		if h.fs.bytes < 0 {
			h.fs.bytes = 0
		}
		h.n.data = h.n.data[:size]
	}
	h.n.mtime = time.Now()
	return nil
}

// Chtimes sets the open file's modification time.
func (h *File) Chtimes(mtime time.Time) error {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.closed {
		return pathErr("chtimes", h.name, fs.ErrClosed)
	}
	if h.n.owner != h.fs.gen {
		return pathErr("chtimes", h.name, ErrReadOnly)
	}
	h.fs.changes++
	h.n.mtime = mtime
	return nil
}

// ReadDir implements fs.ReadDirFile: with n > 0 it returns at most n
// entries and io.EOF at the end; with n <= 0 all remaining entries.
func (h *File) ReadDir(n int) ([]fs.DirEntry, error) {
	ents, err := h.Entries(n)
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, len(ents))
	for i := range ents {
		out[i] = ents[i]
	}
	if n > 0 && len(out) == 0 {
		return nil, io.EOF
	}
	return out, nil
}

// Entries is ReadDir with the concrete type and no io.EOF.
func (h *File) Entries(n int) ([]DirEntry, error) {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.closed {
		return nil, pathErr("readdir", h.name, fs.ErrClosed)
	}
	if !h.n.isDir() {
		return nil, pathErr("readdir", h.name, ErrNotDir)
	}
	if h.dir == nil {
		h.dir = entriesOf(h.n)
	}
	rest := h.dir[h.dirPos:]
	if n > 0 && len(rest) > n {
		rest = rest[:n]
	}
	h.dirPos += len(rest)
	return rest, nil
}

// Close implements io.Closer.
func (h *File) Close() error {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	if h.closed {
		return pathErr("close", h.name, fs.ErrClosed)
	}
	h.closed = true
	return nil
}
