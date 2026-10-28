package image

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// MaxLayoutEntries bounds the entries of a loaded layout tar.
const MaxLayoutEntries = 1 << 14

// ReadLayoutTar reads an OCI image layout sent as a tar (POST /images/load)
// into memory, as an fs.FS that OpenLayout reads. It is untrusted input:
// only regular files and directories under relative, clean names are
// accepted (no links, no "..", no absolute or duplicate names), and the
// whole is bounded by lim.MaxImageBytes, each file by lim.MaxBlobBytes.
// Nothing is read from or written to the host's file system.
func ReadLayoutTar(r io.Reader, lim Limits) (fs.FS, error) {
	m := memFS{}
	tr := tar.NewReader(r)
	var total int64
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: layout tar: %v", ErrInvalid, err)
		}
		if n >= MaxLayoutEntries {
			return nil, fmt.Errorf("%w: layout tar has more than %d entries", ErrTooLarge, MaxLayoutEntries)
		}
		name := strings.TrimPrefix(h.Name, "./")
		name = strings.TrimSuffix(name, "/")
		if name == "" || name == "." {
			continue
		}
		if path.IsAbs(name) || path.Clean(name) != name || name == ".." || strings.HasPrefix(name, "../") || !fs.ValidPath(name) {
			return nil, fmt.Errorf("%w: layout tar: unsafe name %q", ErrInvalid, h.Name)
		}
		if _, dup := m[name]; dup {
			return nil, fmt.Errorf("%w: layout tar: %q twice", ErrInvalid, name)
		}
		switch h.Typeflag {
		case tar.TypeDir:
			m[name] = &memFile{name: path.Base(name), dir: true}
		case tar.TypeReg:
			if h.Size < 0 || h.Size > lim.MaxBlobBytes {
				return nil, fmt.Errorf("%w: layout tar: %s is %d bytes, limit %d", ErrTooLarge, name, h.Size, lim.MaxBlobBytes)
			}
			if total += h.Size; total > lim.MaxImageBytes {
				return nil, fmt.Errorf("%w: layout tar is larger than %d bytes", ErrTooLarge, lim.MaxImageBytes)
			}
			b, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
			if err != nil {
				return nil, fmt.Errorf("%w: layout tar: %v", ErrInvalid, err)
			}
			if int64(len(b)) != h.Size {
				return nil, fmt.Errorf("%w: layout tar: %s is truncated", ErrInvalid, name)
			}
			m[name] = &memFile{name: path.Base(name), data: b}
		default:
			return nil, fmt.Errorf("%w: layout tar: %s is not a file or a directory", ErrInvalid, name)
		}
	}
	return m, nil
}

// memFS is a read-only in-memory fs.FS (directories are implied by the
// names under them).
type memFS map[string]*memFile

type memFile struct {
	name string
	data []byte
	dir  bool
}

func (m memFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
	}
	if f, ok := m[name]; ok && !f.dir {
		return &openFile{memFile: f, r: bytes.NewReader(f.data)}, nil
	}
	// A directory: listed or implied.
	prefix := name + "/"
	if name == "." {
		prefix = ""
	}
	var kids []fs.DirEntry
	seen := map[string]bool{}
	for p, f := range m {
		rest, ok := strings.CutPrefix(p, prefix)
		if !ok || rest == "" {
			continue
		}
		child, deeper, _ := strings.Cut(rest, "/")
		if seen[child] {
			continue
		}
		seen[child] = true
		if deeper != "" {
			kids = append(kids, fs.FileInfoToDirEntry(&memFile{name: child, dir: true}))
		} else {
			kids = append(kids, fs.FileInfoToDirEntry(f))
		}
	}
	if len(kids) == 0 && m[name] == nil && name != "." {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	sort.Slice(kids, func(i, j int) bool { return kids[i].Name() < kids[j].Name() })
	return &openDir{memFile: &memFile{name: path.Base(name), dir: true}, kids: kids}, nil
}

func (f *memFile) Name() string { return f.name }
func (f *memFile) Size() int64  { return int64(len(f.data)) }
func (f *memFile) Mode() fs.FileMode {
	if f.dir {
		return fs.ModeDir | 0o555
	}
	return 0o444
}
func (f *memFile) ModTime() time.Time { return time.Time{} }
func (f *memFile) IsDir() bool        { return f.dir }
func (f *memFile) Sys() any           { return nil }

type openFile struct {
	*memFile
	r *bytes.Reader
}

func (f *openFile) Stat() (fs.FileInfo, error) { return f.memFile, nil }
func (f *openFile) Read(b []byte) (int, error) { return f.r.Read(b) }
func (f *openFile) Close() error               { return nil }

type openDir struct {
	*memFile
	kids []fs.DirEntry
	off  int
}

func (d *openDir) Stat() (fs.FileInfo, error) { return d.memFile, nil }
func (d *openDir) Read([]byte) (int, error) {
	return 0, &fs.PathError{Op: "read", Path: d.name, Err: fs.ErrInvalid}
}
func (d *openDir) Close() error { return nil }
func (d *openDir) ReadDir(n int) ([]fs.DirEntry, error) {
	rest := d.kids[d.off:]
	if n > 0 && len(rest) > n {
		rest = rest[:n]
	}
	d.off += len(rest)
	if n > 0 && len(rest) == 0 {
		return nil, io.EOF
	}
	return rest, nil
}
