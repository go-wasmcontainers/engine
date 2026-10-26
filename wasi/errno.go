package wasi

import (
	"errors"
	"io/fs"

	"github.com/go-wasmcontainers/engine/rootfs"
)

// errno maps a rootfs error to a WASI errno.
func errno(err error) uint32 {
	switch {
	case err == nil:
		return ESUCCESS
	case errors.Is(err, fs.ErrNotExist):
		return ENOENT
	case errors.Is(err, fs.ErrExist):
		return EEXIST
	case errors.Is(err, fs.ErrPermission):
		return EPERM
	case errors.Is(err, fs.ErrClosed), errors.Is(err, rootfs.ErrBadFile):
		return EBADF
	case errors.Is(err, rootfs.ErrNotDir):
		return ENOTDIR
	case errors.Is(err, rootfs.ErrIsDir):
		return EISDIR
	case errors.Is(err, rootfs.ErrNotEmpty):
		return ENOTEMPTY
	case errors.Is(err, rootfs.ErrLoop):
		return ELOOP
	case errors.Is(err, rootfs.ErrReadOnly):
		return EROFS
	case errors.Is(err, rootfs.ErrNameTooLong):
		return ENAMETOOLONG
	case errors.Is(err, rootfs.ErrNoSpace):
		return ENOSPC
	case errors.Is(err, fs.ErrInvalid):
		return EINVAL
	}
	return EIO
}
