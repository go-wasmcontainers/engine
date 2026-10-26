package rootfs

import (
	"archive/tar"
	"io"
	"path"
	"sort"
)

// paxWhiteout marks a diff's whiteouts. OCI layers mark them by name
// (.wh.x), but a guest may create a file named .wh.x, so a diff marks them
// out of band: "file" removes the name from what lies below, "opaque" on a
// directory hides everything below it.
const paxWhiteout = "WCTR.whiteout"

// Usage is what a tree has added, as Quota counts it.
type Usage struct {
	Bytes int64 `json:"bytes"`
	Files int   `json:"files"`
}

// WriteDiff writes what this tree changed relative to the tree it was
// cloned from as a tar changeset that ApplyDiff, on a fresh clone of the
// same base, turns back into an equal tree: added and changed entries in
// full, removed ones as whiteouts, a directory removed and made again as
// opaque. A tree that is not a clone is written whole. Hard links among
// the written entries are kept; a link to an entry left unchanged in the
// base is written as a copy.
func (f *FS) WriteDiff(w io.Writer) (Usage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	tw := tar.NewWriter(w)
	d := differ{tw: tw, seen: map[*node]string{}}
	if f.root != f.base {
		// The root itself, for its mode and time.
		if err := d.write(&tar.Header{Name: "./", Typeflag: tar.TypeDir, Mode: int64(f.root.mode.Perm()), ModTime: f.root.mtime}, nil); err != nil {
			return Usage{}, err
		}
		if err := d.dir(nil, f.root, f.base); err != nil {
			return Usage{}, err
		}
	}
	if err := tw.Close(); err != nil {
		return Usage{}, err
	}
	return Usage{Bytes: f.bytes, Files: f.files}, nil
}

// ApplyDiff applies WriteDiff's output to f, a fresh clone of the same
// base, and restores the usage it recorded (bounded by f's quota). The
// changeset is untrusted (it comes back from storage): it is checked as an
// image layer is, under lim.
func (f *FS) ApplyDiff(r io.Reader, u Usage, lim Limits) error {
	if err := f.applyTar(r, NewBudget(lim), true); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.bytes, f.files = max(u.Bytes, 0), max(u.Files, 0)
	if f.quota.MaxBytes > 0 {
		f.bytes = min(f.bytes, f.quota.MaxBytes)
	}
	if f.quota.MaxFiles > 0 {
		f.files = min(f.files, f.quota.MaxFiles)
	}
	f.changes++
	return nil
}

// Changes counts this tree's mutations: equal counts, equal trees.
func (f *FS) Changes() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.changes
}

type differ struct {
	tw   *tar.Writer
	seen map[*node]string // regular files written, for hard links
}

func sortedNames(n *node) []string {
	names := make([]string, 0, len(n.kids))
	for k := range n.kids {
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// dir writes cur's changes relative to low (nil: everything is new).
func (d *differ) dir(comps []string, cur, low *node) error {
	if low != nil {
		for _, name := range sortedNames(low) {
			if cur.kids[name] == nil {
				if err := d.write(&tar.Header{Name: path.Join(append(comps, name)...), Typeflag: tar.TypeReg, Mode: 0o644,
					PAXRecords: map[string]string{paxWhiteout: "file"}}, nil); err != nil {
					return err
				}
			}
		}
	}
	for _, name := range sortedNames(cur) {
		k := cur.kids[name]
		var lk *node
		if low != nil {
			lk = low.kids[name]
		}
		if k == lk {
			continue // shared with the base: unchanged, all the way down
		}
		sub := append(append([]string{}, comps...), name)
		p := path.Join(sub...)
		hdr := &tar.Header{Name: p, Mode: int64(k.mode.Perm()), ModTime: k.mtime}
		switch {
		case k.isDir():
			hdr.Typeflag = tar.TypeDir
			var below *node
			if lk != nil && lk.isDir() {
				if lk.ino == k.ino {
					below = lk // copied up: only the differences
				} else {
					// Removed and made again: nothing below shows through.
					hdr.PAXRecords = map[string]string{paxWhiteout: "opaque"}
				}
			}
			if err := d.write(hdr, nil); err != nil {
				return err
			}
			if err := d.dir(sub, k, below); err != nil {
				return err
			}
		case k.isSymlink():
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, k.target
			if err := d.write(hdr, nil); err != nil {
				return err
			}
		default:
			if first, ok := d.seen[k]; ok {
				hdr.Typeflag, hdr.Linkname = tar.TypeLink, first
				if err := d.write(hdr, nil); err != nil {
					return err
				}
				continue
			}
			d.seen[k] = p
			hdr.Typeflag, hdr.Size = tar.TypeReg, int64(len(k.data))
			if err := d.write(hdr, k.data); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *differ) write(h *tar.Header, data []byte) error {
	h.Format = tar.FormatPAX // keeps sub-second modification times
	if err := d.tw.WriteHeader(h); err != nil {
		return err
	}
	_, err := d.tw.Write(data)
	return err
}
