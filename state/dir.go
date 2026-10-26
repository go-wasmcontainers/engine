package state

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
)

// Dir is a Store in a directory: one file per key, <root>/<kind>/<name>.
// A Put writes a temporary file, syncs it and renames it over the key, so
// a crash leaves the old value or the new one.
type Dir struct {
	root string
}

// OpenDir opens (creating) a directory store.
func OpenDir(root string) (*Dir, error) {
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	return &Dir{root: root}, nil
}

func (d *Dir) path(k string) (string, error) {
	if err := CheckKey(k); err != nil {
		return "", err
	}
	return filepath.Join(d.root, filepath.FromSlash(k)), nil
}

func (d *Dir) Get(k string) ([]byte, error) {
	p, err := d.path(k)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return b, err
}

func (d *Dir) Put(k string, v []byte) error {
	p, err := d.path(k)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return d.space(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return d.space(err)
	}
	_, err = tmp.Write(v)
	if err == nil {
		err = tmp.Sync()
	}
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp.Name(), p)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return d.space(err)
	}
	return nil
}

func (d *Dir) space(err error) error {
	if errors.Is(err, syscall.ENOSPC) {
		return fmt.Errorf("%w: %v", ErrNoSpace, err)
	}
	return err
}

func (d *Dir) Delete(k string) error {
	p, err := d.path(k)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Describe implements Describer.
func (d *Dir) Describe() (string, int64) {
	var n int64
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			if fi, err := e.Info(); err == nil {
				n += fi.Size()
			}
		}
		return nil
	})
	if err != nil {
		n = -1
	}
	return "directory " + d.root, n
}

func (d *Dir) Keys(prefix string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(d.root, func(p string, e fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if e.IsDir() || strings.HasPrefix(e.Name(), ".tmp-") {
			if strings.HasPrefix(e.Name(), ".tmp-") {
				os.Remove(p) // left by a crash between write and rename
			}
			return nil
		}
		rel, err := filepath.Rel(d.root, p)
		if err != nil {
			return err
		}
		k := filepath.ToSlash(rel)
		if CheckKey(k) == nil && strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}
