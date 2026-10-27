package rootfs

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
)

// ErrArchive is wrapped by every refusal of an archive (PUT/GET
// /containers/{id}/archive).
var ErrArchive = errors.New("rootfs: archive")

// Extract writes a tar into the directory dst of the tree, as `docker cp`
// into a container does. The tar is untrusted: names must be relative and
// clean (no "..", no absolute path), hard links must name an entry inside
// dst, devices and FIFOs are refused, and the entries, each file and the
// total are bounded by lim. Every write goes through the tree's own
// operations, so paths resolve inside the root (a symlink cannot lead
// out) and the tree's quota applies. With noOverwriteDirNonDir, an entry
// that would replace a directory with a non-directory, or the reverse, is
// refused.
func (f *FS) Extract(dst string, r io.Reader, lim Limits, noOverwriteDirNonDir bool) error {
	st, err := f.Stat(dst)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return pathErr("extract", dst, ErrNotDir)
	}
	b := NewBudget(lim)
	tr := tar.NewReader(r)
	type dirTime struct {
		p string
		t time.Time
	}
	var dirs []dirTime
	for n := 0; ; n++ {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("%w: %v", ErrArchive, err)
		}
		if lim.MaxFiles > 0 && n >= lim.MaxFiles {
			return fmt.Errorf("%w: more than %d entries", ErrArchive, lim.MaxFiles)
		}
		rel, err := archiveName(h.Name, lim)
		if err != nil {
			return err
		}
		if rel == "" {
			continue // "./": the destination itself
		}
		p := path.Join(dst, rel)
		if noOverwriteDirNonDir {
			if old, err := f.Lstat(p); err == nil && old.IsDir() != (h.Typeflag == tar.TypeDir) {
				return fmt.Errorf("%w: %s: cannot replace a %s with a %s", ErrArchive, rel, kind(old.IsDir()), kind(h.Typeflag == tar.TypeDir))
			}
		}
		perm := fs.FileMode(h.Mode).Perm()
		switch h.Typeflag {
		case tar.TypeDir:
			if err := f.MkdirAll(p, perm|0o700); err != nil {
				return err
			}
			if err := f.Chmod(p, perm); err != nil {
				return err
			}
			dirs = append(dirs, dirTime{p, h.ModTime})
		case tar.TypeReg:
			if h.Size < 0 || (lim.MaxBytes > 0 && h.Size > lim.MaxBytes) {
				return fmt.Errorf("%w: %s is %d bytes", ErrArchive, rel, h.Size)
			}
			if err := b.spend(h.Size, 1); err != nil {
				return fmt.Errorf("%w: %v", ErrArchive, err)
			}
			data, err := io.ReadAll(io.LimitReader(tr, h.Size+1))
			if err != nil || int64(len(data)) != h.Size {
				return fmt.Errorf("%w: %s is truncated", ErrArchive, rel)
			}
			if err := f.MkdirAll(path.Dir(p), 0o755); err != nil {
				return err
			}
			if err := f.WriteFile(p, data, perm); err != nil {
				return err
			}
			if err := f.Chmod(p, perm); err != nil {
				return err
			}
			f.Chtimes(p, h.ModTime, false)
		case tar.TypeSymlink:
			if len(h.Linkname) > lim.MaxPathLen && lim.MaxPathLen > 0 {
				return fmt.Errorf("%w: %s: link target too long", ErrArchive, rel)
			}
			if err := f.MkdirAll(path.Dir(p), 0o755); err != nil {
				return err
			}
			f.Remove(p) // a symlink replaces what was there, as tar does
			if err := f.Symlink(h.Linkname, p); err != nil {
				return err
			}
		case tar.TypeLink:
			target, err := archiveName(h.Linkname, lim)
			if err != nil || target == "" {
				return fmt.Errorf("%w: %s: hard link to %q", ErrArchive, rel, h.Linkname)
			}
			f.Remove(p)
			if err := f.Link(path.Join(dst, target), p); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: %s: entry type %q is not supported", ErrArchive, rel, h.Typeflag)
		}
	}
	// Directory times last: writing into a directory changes its time.
	for i := len(dirs) - 1; i >= 0; i-- {
		f.Chtimes(dirs[i].p, dirs[i].t, false)
	}
	return nil
}

func kind(dir bool) string {
	if dir {
		return "directory"
	}
	return "non-directory"
}

// archiveName checks an archive entry's name and returns it relative and
// clean ("" for the root).
func archiveName(name string, lim Limits) (string, error) {
	if lim.MaxPathLen > 0 && len(name) > lim.MaxPathLen {
		return "", fmt.Errorf("%w: a name is longer than %d bytes", ErrArchive, lim.MaxPathLen)
	}
	n := strings.TrimPrefix(name, "./")
	n = strings.TrimSuffix(n, "/")
	if n == "" || n == "." {
		return "", nil
	}
	if path.IsAbs(n) || path.Clean(n) != n || n == ".." || strings.HasPrefix(n, "../") || strings.ContainsRune(n, 0) {
		return "", fmt.Errorf("%w: unsafe name %q", ErrArchive, name)
	}
	if lim.MaxDepth > 0 && strings.Count(n, "/")+1 > lim.MaxDepth {
		return "", fmt.Errorf("%w: %q is deeper than %d", ErrArchive, name, lim.MaxDepth)
	}
	return n, nil
}

// WriteTar writes the file or directory at src as a tar rooted at its base
// name, as `docker cp` out of a container does: directories recursively,
// symlinks as links (not followed), each regular file with its content.
func (f *FS) WriteTar(src string, w io.Writer) error {
	in, err := f.Lstat(src)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(w)
	base := path.Base(path.Clean("/" + src))
	if base == "/" {
		base = "."
	}
	var walk func(p, name string, in *Info) error
	walk = func(p, name string, in *Info) error {
		h := &tar.Header{Name: name, Mode: int64(in.Mode().Perm()), ModTime: in.ModTime(), Format: tar.FormatPAX}
		switch {
		case in.IsDir():
			h.Typeflag, h.Name = tar.TypeDir, name+"/"
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			ents, err := f.ReadDir(p)
			if err != nil {
				return err
			}
			for _, e := range ents {
				ci, err := f.Lstat(path.Join(p, e.Name()))
				if err != nil {
					return err
				}
				if err := walk(path.Join(p, e.Name()), path.Join(name, e.Name()), ci); err != nil {
					return err
				}
			}
			return nil
		case in.Mode()&fs.ModeSymlink != 0:
			t, err := f.Readlink(p)
			if err != nil {
				return err
			}
			h.Typeflag, h.Linkname = tar.TypeSymlink, t
			return tw.WriteHeader(h)
		default:
			data, err := f.ReadFile(p)
			if err != nil {
				return err
			}
			h.Typeflag, h.Size = tar.TypeReg, int64(len(data))
			if err := tw.WriteHeader(h); err != nil {
				return err
			}
			_, err = tw.Write(data)
			return err
		}
	}
	if err := walk(src, base, in); err != nil {
		return err
	}
	return tw.Close()
}
