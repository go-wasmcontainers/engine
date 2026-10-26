// Package rootfs is a container's root filesystem: an in-memory tree that
// image layers are applied to, frozen as a read-only lower, and cloned per
// container into a copy-on-write upper.
//
// It is deliberately free of any WebAssembly runtime type, so the native
// executor (wazero) and the browser executor can both adapt the same tree.
//
// The tree lives entirely in memory and every path is resolved inside it:
// absolute symlink targets are relative to the tree's root and ".." stops
// at the root, so nothing a container or a layer does can reach the host.
package rootfs

import (
	"errors"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Errors beyond those io/fs defines.
var (
	ErrNotDir      = errors.New("not a directory")
	ErrIsDir       = errors.New("is a directory")
	ErrNotEmpty    = errors.New("directory not empty")
	ErrLoop        = errors.New("too many levels of symbolic links")
	ErrNoSpace     = errors.New("no space left on device")
	ErrReadOnly    = errors.New("read-only file system")
	ErrNameTooLong = errors.New("file name too long")
	ErrBadFile     = errors.New("bad file descriptor")
	ErrCrossLink   = errors.New("invalid cross-device link")
)

// maxSymlinkHops matches Linux's MAXSYMLINKS.
const maxSymlinkHops = 40

// maxPath bounds every path argument.
const maxPath = 4096

var (
	genSeq atomic.Uint64
	inoSeq atomic.Uint64
)

type node struct {
	mode   fs.FileMode // type bits and permissions
	mtime  time.Time
	data   []byte           // regular file
	target string           // symlink
	kids   map[string]*node // directory
	owner  uint64           // generation that may mutate it in place
	ino    uint64
	nlink  int32 // hard links, for regular files
}

func (n *node) isDir() bool     { return n.mode.IsDir() }
func (n *node) isSymlink() bool { return n.mode&fs.ModeSymlink != 0 }
func (n *node) isRegular() bool { return n.mode.IsRegular() }

// Quota bounds what may be written into a tree.
type Quota struct {
	// MaxBytes bounds the bytes this tree adds (0 = unlimited).
	MaxBytes int64
	// MaxFiles bounds the entries this tree adds (0 = unlimited).
	MaxFiles int
}

// FS is an in-memory filesystem tree. It is safe for concurrent use.
type FS struct {
	mu     sync.Mutex
	root   *node
	gen    uint64
	frozen bool
	quota  Quota
	bytes  int64
	files  int
	// base is the root this tree was cloned from (nil if not a clone),
	// which WriteDiff compares against.
	base *node
	// changes counts mutations (see Changes).
	changes uint64
}

// New returns an empty tree with a 0755 root directory.
func New() *FS {
	f := &FS{gen: genSeq.Add(1)}
	f.root = f.newNode(fs.ModeDir | 0o755)
	f.root.kids = map[string]*node{}
	return f
}

func (f *FS) newNode(mode fs.FileMode) *node {
	n := &node{mode: mode, mtime: time.Now(), owner: f.gen, ino: inoSeq.Add(1), nlink: 1}
	if mode.IsDir() {
		n.kids = map[string]*node{}
	}
	return n
}

// Freeze makes the tree read-only: every later mutation fails with
// ErrReadOnly. Clones of a frozen tree are writable.
func (f *FS) Freeze() {
	f.mu.Lock()
	f.frozen = true
	f.mu.Unlock()
}

// Clone returns a writable copy-on-write view of f: f's nodes are shared
// until the clone writes to them, at which point the clone copies them up.
// Writing to f afterwards (if it is not frozen) copies up as well, so
// neither ever sees the other's changes.
func (f *FS) Clone(q Quota) *FS {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.frozen {
		f.gen = genSeq.Add(1)
	}
	return &FS{root: f.root, base: f.root, gen: genSeq.Add(1), quota: q}
}

// Usage returns the bytes and entries this tree has added.
func (f *FS) Usage() (bytes int64, files int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bytes, f.files
}

func pathErr(op, p string, err error) error { return &fs.PathError{Op: op, Path: p, Err: err} }

// split cleans p into root-relative components. "" and "/" are the root.
func split(p string) ([]string, error) {
	if len(p) > maxPath {
		return nil, ErrNameTooLong
	}
	if strings.IndexByte(p, 0) >= 0 {
		return nil, fs.ErrInvalid
	}
	var out []string
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." {
			continue
		}
		if len(c) > 255 {
			return nil, ErrNameTooLong
		}
		out = append(out, c)
	}
	return out, nil
}

// resolved is the result of a path walk.
type resolved struct {
	comps []string // canonical components of the final path
	node  *node    // nil when the final component does not exist
}

// resolveLocked walks p from the root, following symlinks in every
// component, and in the last one only when followLast is set. ".." never
// climbs above the root.
func (f *FS) resolveLocked(p string, followLast bool) (resolved, error) {
	queue, err := split(p)
	if err != nil {
		return resolved{}, err
	}
	comps := make([]string, 0, len(queue))
	stack := []*node{f.root}
	hops := 0
	for len(queue) > 0 {
		c := queue[0]
		queue = queue[1:]
		if c == ".." {
			if len(comps) > 0 {
				comps = comps[:len(comps)-1]
				stack = stack[:len(stack)-1]
			}
			continue
		}
		cur := stack[len(stack)-1]
		if !cur.isDir() {
			return resolved{}, ErrNotDir
		}
		child := cur.kids[c]
		last := len(queue) == 0
		if child == nil {
			if last {
				return resolved{comps: append(comps, c)}, nil
			}
			return resolved{}, fs.ErrNotExist
		}
		if child.isSymlink() && (!last || followLast) {
			hops++
			if hops > maxSymlinkHops {
				return resolved{}, ErrLoop
			}
			tc, err := split(child.target)
			if err != nil {
				return resolved{}, err
			}
			if strings.HasPrefix(child.target, "/") {
				comps = comps[:0]
				stack = stack[:1]
			}
			queue = append(tc, queue...)
			continue
		}
		comps = append(comps, c)
		stack = append(stack, child)
	}
	return resolved{comps: comps, node: stack[len(stack)-1]}, nil
}

// mutDirLocked returns the directory at canonical comps, copying it and
// its ancestors up into this generation so it may be changed in place.
func (f *FS) mutDirLocked(comps []string) (*node, error) {
	if f.frozen {
		return nil, ErrReadOnly
	}
	f.changes++
	if f.root.owner != f.gen {
		f.root = f.copyUp(f.root)
	}
	cur := f.root
	for _, c := range comps {
		child := cur.kids[c]
		if child == nil {
			return nil, fs.ErrNotExist
		}
		if !child.isDir() {
			return nil, ErrNotDir
		}
		if child.owner != f.gen {
			child = f.copyUp(child)
			cur.kids[c] = child
		}
		cur = child
	}
	return cur, nil
}

// copyUp copies n into this generation. Directory maps are copied (their
// entries still shared); file content is copied.
func (f *FS) copyUp(n *node) *node {
	c := *n
	c.owner = f.gen
	if n.kids != nil {
		c.kids = make(map[string]*node, len(n.kids))
		for k, v := range n.kids {
			c.kids[k] = v
		}
	}
	if n.data != nil {
		c.data = append([]byte(nil), n.data...)
	}
	return &c
}

// mutChildLocked returns the owned copy of the entry name in the owned
// directory dir.
func (f *FS) mutChildLocked(dir *node, name string) *node {
	n := dir.kids[name]
	if n != nil && n.owner != f.gen {
		n = f.copyUp(n)
		dir.kids[name] = n
	}
	return n
}

func (f *FS) chargeLocked(bytes int64, files int) error {
	if f.quota.MaxBytes > 0 && bytes > 0 && f.bytes+bytes > f.quota.MaxBytes {
		return ErrNoSpace
	}
	if f.quota.MaxFiles > 0 && files > 0 && f.files+files > f.quota.MaxFiles {
		return ErrNoSpace
	}
	f.bytes += bytes
	f.files += files
	return nil
}

// Info describes a file. It implements fs.FileInfo.
type Info struct {
	name  string
	size  int64
	mode  fs.FileMode
	mtime time.Time
	Ino   uint64
	Nlink uint64
}

func (i *Info) Name() string       { return i.name }
func (i *Info) Size() int64        { return i.size }
func (i *Info) Mode() fs.FileMode  { return i.mode }
func (i *Info) ModTime() time.Time { return i.mtime }
func (i *Info) IsDir() bool        { return i.mode.IsDir() }
func (i *Info) Sys() any           { return nil }

func infoOf(name string, n *node) *Info {
	in := &Info{name: name, mode: n.mode, mtime: n.mtime, Ino: n.ino, Nlink: uint64(n.nlink)}
	switch {
	case n.isRegular():
		in.size = int64(len(n.data))
	case n.isSymlink():
		in.size = int64(len(n.target))
	case n.isDir():
		in.Nlink = 2
	}
	if in.Nlink == 0 {
		in.Nlink = 1
	}
	if name == "" {
		in.name = "/"
	}
	return in
}

func baseOf(comps []string) string {
	if len(comps) == 0 {
		return ""
	}
	return comps[len(comps)-1]
}

// Stat describes the file at p, following symlinks.
func (f *FS) Stat(p string) (*Info, error) { return f.stat("stat", p, true) }

// Lstat describes the file at p without following a final symlink.
func (f *FS) Lstat(p string) (*Info, error) { return f.stat("lstat", p, false) }

func (f *FS) stat(op, p string, follow bool) (*Info, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.resolveLocked(p, follow)
	if err != nil {
		return nil, pathErr(op, p, err)
	}
	if r.node == nil {
		return nil, pathErr(op, p, fs.ErrNotExist)
	}
	return infoOf(baseOf(r.comps), r.node), nil
}

// DirEntry is one directory entry. It implements fs.DirEntry.
type DirEntry struct {
	info *Info
}

func (d DirEntry) Name() string               { return d.info.name }
func (d DirEntry) IsDir() bool                { return d.info.IsDir() }
func (d DirEntry) Type() fs.FileMode          { return d.info.mode.Type() }
func (d DirEntry) Info() (fs.FileInfo, error) { return d.info, nil }

// Ino returns the entry's inode number.
func (d DirEntry) Ino() uint64 { return d.info.Ino }

func entriesOf(n *node) []DirEntry {
	names := make([]string, 0, len(n.kids))
	for k := range n.kids {
		names = append(names, k)
	}
	sort.Strings(names)
	out := make([]DirEntry, len(names))
	for i, k := range names {
		out[i] = DirEntry{info: infoOf(k, n.kids[k])}
	}
	return out
}

// ReadDir lists the directory at p, sorted by name.
func (f *FS) ReadDir(p string) ([]DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.resolveLocked(p, true)
	if err != nil {
		return nil, pathErr("readdir", p, err)
	}
	if r.node == nil {
		return nil, pathErr("readdir", p, fs.ErrNotExist)
	}
	if !r.node.isDir() {
		return nil, pathErr("readdir", p, ErrNotDir)
	}
	return entriesOf(r.node), nil
}

// ReadFile returns a copy of the content of the file at p.
func (f *FS) ReadFile(p string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.resolveLocked(p, true)
	if err != nil {
		return nil, pathErr("read", p, err)
	}
	if r.node == nil {
		return nil, pathErr("read", p, fs.ErrNotExist)
	}
	if r.node.isDir() {
		return nil, pathErr("read", p, ErrIsDir)
	}
	return append([]byte(nil), r.node.data...), nil
}

// WriteFile creates or truncates the file at p and writes data to it.
func (f *FS) WriteFile(p string, data []byte, perm fs.FileMode) error {
	h, err := f.OpenFile(p, O_WRONLY|O_CREATE|O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := h.Write(data); err != nil {
		h.Close()
		return err
	}
	return h.Close()
}

// parentLocked resolves the parent of p (following symlinks) and returns
// its canonical components and the final name.
func (f *FS) parentLocked(p string) ([]string, string, error) {
	comps, err := split(p)
	if err != nil {
		return nil, "", err
	}
	if len(comps) == 0 {
		return nil, "", fs.ErrInvalid // the root has no parent
	}
	name := comps[len(comps)-1]
	if name == ".." {
		return nil, "", fs.ErrInvalid
	}
	r, err := f.resolveLocked(strings.Join(comps[:len(comps)-1], "/"), true)
	if err != nil {
		return nil, "", err
	}
	if r.node == nil {
		return nil, "", fs.ErrNotExist
	}
	if !r.node.isDir() {
		return nil, "", ErrNotDir
	}
	return r.comps, name, nil
}

// Mkdir creates a directory.
func (f *FS) Mkdir(p string, perm fs.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pc, name, err := f.parentLocked(p)
	if err != nil {
		return pathErr("mkdir", p, err)
	}
	dir, err := f.mutDirLocked(pc)
	if err != nil {
		return pathErr("mkdir", p, err)
	}
	if dir.kids[name] != nil {
		return pathErr("mkdir", p, fs.ErrExist)
	}
	if err := f.chargeLocked(0, 1); err != nil {
		return pathErr("mkdir", p, err)
	}
	dir.kids[name] = f.newNode(fs.ModeDir | perm.Perm())
	dir.mtime = time.Now()
	return nil
}

// MkdirAll creates p and any missing parents.
func (f *FS) MkdirAll(p string, perm fs.FileMode) error {
	comps, err := split(p)
	if err != nil {
		return pathErr("mkdir", p, err)
	}
	for i := range comps {
		sub := strings.Join(comps[:i+1], "/")
		if err := f.Mkdir(sub, perm); err != nil && !errors.Is(err, fs.ErrExist) {
			return err
		}
		if st, err := f.Stat(sub); err != nil {
			return err
		} else if !st.IsDir() {
			return pathErr("mkdir", sub, ErrNotDir)
		}
	}
	return nil
}

// Remove unlinks a non-directory entry (a symlink itself, not its target).
func (f *FS) Remove(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pc, name, err := f.parentLocked(p)
	if err != nil {
		return pathErr("unlink", p, err)
	}
	dir, err := f.mutDirLocked(pc)
	if err != nil {
		return pathErr("unlink", p, err)
	}
	n := dir.kids[name]
	if n == nil {
		return pathErr("unlink", p, fs.ErrNotExist)
	}
	if n.isDir() {
		return pathErr("unlink", p, ErrIsDir)
	}
	f.releaseLocked(n)
	delete(dir.kids, name)
	dir.mtime = time.Now()
	return nil
}

func (f *FS) releaseLocked(n *node) {
	if n.owner == f.gen {
		if n.nlink > 1 {
			n.nlink--
			return
		}
		f.bytes -= int64(len(n.data))
		if f.bytes < 0 {
			f.bytes = 0
		}
	}
	if f.files > 0 {
		f.files--
	}
}

// Rmdir removes an empty directory.
func (f *FS) Rmdir(p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	pc, name, err := f.parentLocked(p)
	if err != nil {
		return pathErr("rmdir", p, err)
	}
	dir, err := f.mutDirLocked(pc)
	if err != nil {
		return pathErr("rmdir", p, err)
	}
	n := dir.kids[name]
	if n == nil {
		return pathErr("rmdir", p, fs.ErrNotExist)
	}
	if !n.isDir() {
		return pathErr("rmdir", p, ErrNotDir)
	}
	if len(n.kids) > 0 {
		return pathErr("rmdir", p, ErrNotEmpty)
	}
	delete(dir.kids, name)
	if f.files > 0 {
		f.files--
	}
	dir.mtime = time.Now()
	return nil
}

// Rename moves from to to, replacing to as POSIX rename does.
func (f *FS) Rename(from, to string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	fpc, fname, err := f.parentLocked(from)
	if err != nil {
		return pathErr("rename", from, err)
	}
	tpc, tname, err := f.parentLocked(to)
	if err != nil {
		return pathErr("rename", to, err)
	}
	fdir, err := f.mutDirLocked(fpc)
	if err != nil {
		return pathErr("rename", from, err)
	}
	src := fdir.kids[fname]
	if src == nil {
		return pathErr("rename", from, fs.ErrNotExist)
	}
	fullFrom := append(append([]string{}, fpc...), fname)
	fullTo := append(append([]string{}, tpc...), tname)
	if path.Join(fullFrom...) == path.Join(fullTo...) {
		return nil
	}
	if src.isDir() && len(fullTo) > len(fullFrom) && path.Join(fullTo[:len(fullFrom)]...) == path.Join(fullFrom...) {
		return pathErr("rename", to, fs.ErrInvalid) // into its own subtree
	}
	tdir, err := f.mutDirLocked(tpc)
	if err != nil {
		return pathErr("rename", to, err)
	}
	if dst := tdir.kids[tname]; dst != nil {
		switch {
		case src.isDir() && !dst.isDir():
			return pathErr("rename", to, ErrNotDir)
		case !src.isDir() && dst.isDir():
			return pathErr("rename", to, ErrIsDir)
		case dst.isDir() && len(dst.kids) > 0:
			return pathErr("rename", to, ErrNotEmpty)
		}
		f.releaseLocked(dst)
	}
	// fdir may have been replaced by copy-up only if it was not owned, and
	// mutDirLocked(fpc) made it owned, so the pointer is still the live one.
	delete(fdir.kids, fname)
	tdir.kids[tname] = src
	now := time.Now()
	fdir.mtime, tdir.mtime = now, now
	return nil
}

// Symlink creates linkPath pointing at target.
func (f *FS) Symlink(target, linkPath string) error {
	if len(target) > maxPath || strings.IndexByte(target, 0) >= 0 || target == "" {
		return pathErr("symlink", linkPath, fs.ErrInvalid)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	pc, name, err := f.parentLocked(linkPath)
	if err != nil {
		return pathErr("symlink", linkPath, err)
	}
	dir, err := f.mutDirLocked(pc)
	if err != nil {
		return pathErr("symlink", linkPath, err)
	}
	if dir.kids[name] != nil {
		return pathErr("symlink", linkPath, fs.ErrExist)
	}
	if err := f.chargeLocked(int64(len(target)), 1); err != nil {
		return pathErr("symlink", linkPath, err)
	}
	n := f.newNode(fs.ModeSymlink | 0o777)
	n.target = target
	dir.kids[name] = n
	return nil
}

// Readlink returns a symlink's target.
func (f *FS) Readlink(p string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.resolveLocked(p, false)
	if err != nil {
		return "", pathErr("readlink", p, err)
	}
	if r.node == nil {
		return "", pathErr("readlink", p, fs.ErrNotExist)
	}
	if !r.node.isSymlink() {
		return "", pathErr("readlink", p, fs.ErrInvalid)
	}
	return r.node.target, nil
}

// Link creates newPath as a hard link to the non-directory oldPath.
func (f *FS) Link(oldPath, newPath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.resolveLocked(oldPath, false)
	if err != nil {
		return pathErr("link", oldPath, err)
	}
	if r.node == nil {
		return pathErr("link", oldPath, fs.ErrNotExist)
	}
	if r.node.isDir() {
		return pathErr("link", oldPath, fs.ErrPermission)
	}
	pc, name, err := f.parentLocked(newPath)
	if err != nil {
		return pathErr("link", newPath, err)
	}
	// Own the source so both names share one mutable node.
	opc := r.comps[:len(r.comps)-1]
	odir, err := f.mutDirLocked(opc)
	if err != nil {
		return pathErr("link", oldPath, err)
	}
	src := f.mutChildLocked(odir, baseOf(r.comps))
	dir, err := f.mutDirLocked(pc)
	if err != nil {
		return pathErr("link", newPath, err)
	}
	if dir.kids[name] != nil {
		return pathErr("link", newPath, fs.ErrExist)
	}
	if err := f.chargeLocked(0, 1); err != nil {
		return pathErr("link", newPath, err)
	}
	src.nlink++
	dir.kids[name] = src
	return nil
}

// Chmod changes permission bits.
func (f *FS) Chmod(p string, mode fs.FileMode) error {
	return f.modify("chmod", p, true, func(n *node) { n.mode = n.mode.Type() | mode.Perm() })
}

// Chtimes sets the modification time (access time is not kept).
func (f *FS) Chtimes(p string, mtime time.Time, follow bool) error {
	return f.modify("chtimes", p, follow, func(n *node) { n.mtime = mtime })
}

func (f *FS) modify(op, p string, follow bool, fn func(*node)) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, err := f.resolveLocked(p, follow)
	if err != nil {
		return pathErr(op, p, err)
	}
	if r.node == nil {
		return pathErr(op, p, fs.ErrNotExist)
	}
	if len(r.comps) == 0 {
		if f.frozen {
			return pathErr(op, p, ErrReadOnly)
		}
		f.changes++
		if f.root.owner != f.gen {
			f.root = f.copyUp(f.root)
		}
		fn(f.root)
		return nil
	}
	dir, err := f.mutDirLocked(r.comps[:len(r.comps)-1])
	if err != nil {
		return pathErr(op, p, err)
	}
	fn(f.mutChildLocked(dir, baseOf(r.comps)))
	return nil
}
