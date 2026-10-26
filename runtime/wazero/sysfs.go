package wazero

import (
	"errors"
	"io"
	"io/fs"
	"time"

	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"
	"github.com/tetratelabs/wazero/sys"

	"github.com/go-wasmcontainers/engine/rootfs"
)

// sysFS adapts a container's rootfs.FS to wazero's writable filesystem
// interface (experimental/sys.FS), mounted as the guest's "/".
type sysFS struct {
	experimentalsys.UnimplementedFS
	fs *rootfs.FS
}

func errno(err error) experimentalsys.Errno {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, fs.ErrNotExist):
		return experimentalsys.ENOENT
	case errors.Is(err, fs.ErrExist):
		return experimentalsys.EEXIST
	case errors.Is(err, fs.ErrPermission):
		return experimentalsys.EPERM
	case errors.Is(err, fs.ErrClosed), errors.Is(err, rootfs.ErrBadFile):
		return experimentalsys.EBADF
	case errors.Is(err, rootfs.ErrNotDir):
		return experimentalsys.ENOTDIR
	case errors.Is(err, rootfs.ErrIsDir):
		return experimentalsys.EISDIR
	case errors.Is(err, rootfs.ErrNotEmpty):
		return experimentalsys.ENOTEMPTY
	case errors.Is(err, rootfs.ErrLoop):
		return experimentalsys.ELOOP
	case errors.Is(err, rootfs.ErrReadOnly):
		return experimentalsys.EROFS
	case errors.Is(err, rootfs.ErrNameTooLong):
		return experimentalsys.ENAMETOOLONG
	case errors.Is(err, fs.ErrInvalid):
		return experimentalsys.EINVAL
	case errors.Is(err, rootfs.ErrNoSpace):
		// wazero's Errno set has no ENOSPC; EIO is the closest it carries.
		return experimentalsys.EIO
	}
	return experimentalsys.EIO
}

func toFlag(f experimentalsys.Oflag) rootfs.Flag {
	var out rootfs.Flag
	switch {
	case f&experimentalsys.O_RDWR != 0:
		out = rootfs.O_RDWR
	case f&experimentalsys.O_WRONLY != 0:
		out = rootfs.O_WRONLY
	}
	for _, m := range []struct {
		in  experimentalsys.Oflag
		out rootfs.Flag
	}{
		{experimentalsys.O_APPEND, rootfs.O_APPEND},
		{experimentalsys.O_CREAT, rootfs.O_CREATE},
		{experimentalsys.O_EXCL, rootfs.O_EXCL},
		{experimentalsys.O_TRUNC, rootfs.O_TRUNC},
		{experimentalsys.O_DIRECTORY, rootfs.O_DIRECTORY},
		{experimentalsys.O_NOFOLLOW, rootfs.O_NOFOLLOW},
	} {
		if f&m.in != 0 {
			out |= m.out
		}
	}
	return out
}

func stat(in *rootfs.Info) sys.Stat_t {
	t := in.ModTime().UnixNano()
	return sys.Stat_t{Dev: 1, Ino: in.Ino, Mode: in.Mode(), Nlink: in.Nlink, Size: in.Size(), Atim: t, Mtim: t, Ctim: t}
}

func (s *sysFS) OpenFile(path string, flag experimentalsys.Oflag, perm fs.FileMode) (experimentalsys.File, experimentalsys.Errno) {
	h, err := s.fs.OpenFile(path, toFlag(flag), perm)
	if err != nil {
		return nil, errno(err)
	}
	return &sysFile{h: h}, 0
}

func (s *sysFS) Lstat(path string) (sys.Stat_t, experimentalsys.Errno) {
	in, err := s.fs.Lstat(path)
	if err != nil {
		return sys.Stat_t{}, errno(err)
	}
	return stat(in), 0
}

func (s *sysFS) Stat(path string) (sys.Stat_t, experimentalsys.Errno) {
	in, err := s.fs.Stat(path)
	if err != nil {
		return sys.Stat_t{}, errno(err)
	}
	return stat(in), 0
}

func (s *sysFS) Mkdir(path string, perm fs.FileMode) experimentalsys.Errno {
	return errno(s.fs.Mkdir(path, perm))
}

func (s *sysFS) Chmod(path string, perm fs.FileMode) experimentalsys.Errno {
	return errno(s.fs.Chmod(path, perm))
}

func (s *sysFS) Rename(from, to string) experimentalsys.Errno {
	return errno(s.fs.Rename(from, to))
}

func (s *sysFS) Rmdir(path string) experimentalsys.Errno { return errno(s.fs.Rmdir(path)) }

func (s *sysFS) Unlink(path string) experimentalsys.Errno {
	err := s.fs.Remove(path)
	if errors.Is(err, rootfs.ErrIsDir) {
		// unlink(2) on a directory is EISDIR on Linux, EPERM by POSIX;
		// WASI's path_unlink_file expects EISDIR.
		return experimentalsys.EISDIR
	}
	return errno(err)
}

func (s *sysFS) Link(oldPath, newPath string) experimentalsys.Errno {
	return errno(s.fs.Link(oldPath, newPath))
}

func (s *sysFS) Symlink(oldPath, linkName string) experimentalsys.Errno {
	return errno(s.fs.Symlink(oldPath, linkName))
}

func (s *sysFS) Readlink(path string) (string, experimentalsys.Errno) {
	t, err := s.fs.Readlink(path)
	return t, errno(err)
}

func (s *sysFS) Utimens(path string, atim, mtim int64) experimentalsys.Errno {
	if mtim == experimentalsys.UTIME_OMIT {
		if _, err := s.fs.Stat(path); err != nil {
			return errno(err)
		}
		return 0
	}
	return errno(s.fs.Chtimes(path, time.Unix(0, mtim), true))
}

// sysFile adapts an open rootfs.File.
type sysFile struct {
	experimentalsys.UnimplementedFile
	h *rootfs.File
}

func (f *sysFile) Dev() (uint64, experimentalsys.Errno) { return 1, 0 }

func (f *sysFile) Ino() (sys.Inode, experimentalsys.Errno) {
	in, err := f.h.Info()
	if err != nil {
		return 0, errno(err)
	}
	return in.Ino, 0
}

func (f *sysFile) IsDir() (bool, experimentalsys.Errno) { return f.h.IsDir(), 0 }
func (f *sysFile) IsAppend() bool                       { return f.h.IsAppend() }

func (f *sysFile) SetAppend(enable bool) experimentalsys.Errno {
	f.h.SetAppend(enable)
	return 0
}

func (f *sysFile) Stat() (sys.Stat_t, experimentalsys.Errno) {
	in, err := f.h.Info()
	if err != nil {
		return sys.Stat_t{}, errno(err)
	}
	return stat(in), 0
}

func (f *sysFile) Read(buf []byte) (int, experimentalsys.Errno) {
	n, err := f.h.Read(buf)
	if err == io.EOF {
		return n, 0
	}
	return n, errno(err)
}

func (f *sysFile) Pread(buf []byte, off int64) (int, experimentalsys.Errno) {
	n, err := f.h.ReadAt(buf, off)
	if err == io.EOF {
		return n, 0
	}
	return n, errno(err)
}

func (f *sysFile) Seek(offset int64, whence int) (int64, experimentalsys.Errno) {
	n, err := f.h.Seek(offset, whence)
	return n, errno(err)
}

func (f *sysFile) Readdir(n int) ([]experimentalsys.Dirent, experimentalsys.Errno) {
	ents, err := f.h.Entries(n)
	if err != nil {
		return nil, errno(err)
	}
	out := make([]experimentalsys.Dirent, len(ents))
	for i, e := range ents {
		out[i] = experimentalsys.Dirent{Ino: e.Ino(), Name: e.Name(), Type: e.Type()}
	}
	return out, 0
}

func (f *sysFile) Write(buf []byte) (int, experimentalsys.Errno) {
	n, err := f.h.Write(buf)
	return n, errno(err)
}

func (f *sysFile) Pwrite(buf []byte, off int64) (int, experimentalsys.Errno) {
	n, err := f.h.WriteAt(buf, off)
	return n, errno(err)
}

func (f *sysFile) Truncate(size int64) experimentalsys.Errno { return errno(f.h.Truncate(size)) }

func (f *sysFile) Utimens(atim, mtim int64) experimentalsys.Errno {
	if mtim == experimentalsys.UTIME_OMIT {
		return 0
	}
	return errno(f.h.Chtimes(time.Unix(0, mtim)))
}

func (f *sysFile) Close() experimentalsys.Errno {
	err := f.h.Close()
	if errors.Is(err, fs.ErrClosed) {
		return 0
	}
	return errno(err)
}
