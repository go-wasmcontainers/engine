package rootfs

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"strings"
	"time"
)

// Layer media types (kept here so the package does not import image).
const (
	mtTar      = "application/vnd.oci.image.layer.v1.tar"
	mtTarGz    = "application/vnd.oci.image.layer.v1.tar+gzip"
	mtDockerGz = "application/vnd.docker.image.rootfs.diff.tar.gzip"
	mtWasm     = "application/wasm"
)

const (
	whiteoutPrefix = ".wh."
	opaqueMarker   = ".wh..wh..opq"
)

// Limits bounds what unpacking an image's layers may produce. A Budget
// spends one Limits across all the layers of an image.
type Limits struct {
	// MaxBytes bounds the total uncompressed bytes of file content.
	MaxBytes int64
	// MaxFiles bounds the total number of entries.
	MaxFiles int
	// MaxRatio bounds decompressed/compressed bytes of a compressed layer.
	MaxRatio int64
	// MaxPathLen bounds an entry name or link target.
	MaxPathLen int
	// MaxDepth bounds the number of components of an entry name.
	MaxDepth int
}

// DefaultLimits fit wasm images with a wide margin.
var DefaultLimits = Limits{
	MaxBytes:   1 << 30,
	MaxFiles:   1 << 17,
	MaxRatio:   200,
	MaxPathLen: 4096,
	MaxDepth:   128,
}

// ErrLayer is wrapped by every refusal of layer content.
var ErrLayer = errors.New("unsafe or invalid layer")

func layerErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrLayer, fmt.Sprintf(format, a...))
}

// Budget tracks what has been spent of a Limits.
type Budget struct {
	Limits
	Bytes int64
	Files int
}

// NewBudget starts spending lim.
func NewBudget(lim Limits) *Budget { return &Budget{Limits: lim} }

func (b *Budget) spend(bytes int64, files int) error {
	if bytes < 0 || b.Bytes+bytes > b.MaxBytes {
		return layerErr("unpacked content exceeds %d bytes", b.MaxBytes)
	}
	if b.Files+files > b.MaxFiles {
		return layerErr("unpacked content exceeds %d entries", b.MaxFiles)
	}
	b.Bytes += bytes
	b.Files += files
	return nil
}

// countingReader counts bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ratioReader fails once more than ratio times the compressed input (plus
// a fixed slack for tiny inputs) has come out of the decompressor.
type ratioReader struct {
	r     io.Reader
	in    *countingReader
	out   int64
	ratio int64
}

const ratioSlack = 1 << 20

func (z *ratioReader) Read(p []byte) (int, error) {
	n, err := z.r.Read(p)
	z.out += int64(n)
	if z.out > z.in.n*z.ratio+ratioSlack {
		return n, layerErr("decompression ratio above %d:1", z.ratio)
	}
	return n, err
}

// ApplyLayer applies one layer blob of the given media type to f. title is
// the layer's org.opencontainers.image.title annotation, used to name a
// bare wasm module.
func (f *FS) ApplyLayer(r io.Reader, mediaType, title string, b *Budget) error {
	switch mediaType {
	case mtTar:
		return f.ApplyTar(r, b)
	case mtTarGz, mtDockerGz:
		in := &countingReader{r: r}
		zr, err := gzip.NewReader(in)
		if err != nil {
			return layerErr("gzip: %v", err)
		}
		defer zr.Close()
		return f.ApplyTar(&ratioReader{r: zr, in: in, ratio: b.MaxRatio}, b)
	case mtWasm:
		_, err := f.ApplyWasm(r, title, b)
		return err
	}
	return layerErr("unsupported layer media type %q", mediaType)
}

// wasmMagic opens every WebAssembly binary.
var wasmMagic = []byte{0, 'a', 's', 'm'}

// ModulePath is where a bare wasm module layer is placed: "/" plus its
// title when that is a plain, safe file name, otherwise "/module.wasm".
func ModulePath(title string) string {
	if title != "" && len(title) <= 255 && !strings.ContainsAny(title, "/\\\x00") &&
		title != "." && title != ".." && !strings.HasPrefix(title, whiteoutPrefix) {
		return "/" + title
	}
	return "/module.wasm"
}

// ApplyWasm writes a single wasm module (a CNCF wasm artifact layer) into
// the tree and returns its path.
func (f *FS) ApplyWasm(r io.Reader, title string, b *Budget) (string, error) {
	remain := b.MaxBytes - b.Bytes
	data, err := io.ReadAll(io.LimitReader(r, remain+1))
	if err != nil {
		return "", err
	}
	if int64(len(data)) > remain {
		return "", layerErr("wasm module exceeds %d bytes", b.MaxBytes)
	}
	if !bytes.HasPrefix(data, wasmMagic) {
		return "", layerErr("application/wasm layer is not a WebAssembly binary")
	}
	if err := b.spend(int64(len(data)), 1); err != nil {
		return "", err
	}
	p := ModulePath(title)
	if err := f.WriteFile(p, data, 0o755); err != nil {
		return "", err
	}
	return p, nil
}

// entryName validates a tar entry name and returns its components.
func entryName(name string, lim Limits) ([]string, error) {
	if len(name) > lim.MaxPathLen {
		return nil, layerErr("entry name longer than %d bytes", lim.MaxPathLen)
	}
	if strings.IndexByte(name, 0) >= 0 {
		return nil, layerErr("entry name %q contains NUL", name)
	}
	if strings.HasPrefix(name, "/") {
		return nil, layerErr("absolute entry name %q", name)
	}
	var comps []string
	for _, c := range strings.Split(name, "/") {
		switch c {
		case "", ".":
			continue
		case "..":
			return nil, layerErr("entry name %q climbs out with ..", name)
		}
		if len(c) > 255 {
			return nil, layerErr("entry name component longer than 255 bytes")
		}
		comps = append(comps, c)
	}
	if len(comps) > lim.MaxDepth {
		return nil, layerErr("entry name %q deeper than %d", name, lim.MaxDepth)
	}
	return comps, nil
}

// checkSymlinkTarget refuses a target that, resolved lexically from the
// link's directory, would climb above the root.
func checkSymlinkTarget(dir []string, target string, lim Limits) error {
	if target == "" || len(target) > lim.MaxPathLen || strings.IndexByte(target, 0) >= 0 {
		return layerErr("invalid symlink target %q", target)
	}
	depth := len(dir)
	if strings.HasPrefix(target, "/") {
		depth = 0
	}
	for _, c := range strings.Split(target, "/") {
		switch c {
		case "", ".":
		case "..":
			if depth == 0 {
				return layerErr("symlink target %q escapes the root", target)
			}
			depth--
		default:
			depth++
		}
	}
	return nil
}

type staged struct {
	hdr   *tar.Header
	comps []string
	data  []byte
	link  []string // hard link target
}

// ApplyTar applies an uncompressed tar changeset to f, per the OCI layer
// specification: opaque whiteouts first, then explicit whiteouts, both
// against what lower layers left; then additions in archive order, so a
// whiteout never hides an entry of its own layer.
//
// Refused outright: absolute names, "..", names under a non-directory
// (which is how a symlink planted by one entry redirects the next), symlink
// targets that climb above the root, hard links to anything but an
// existing regular file, and anything over the budget.
func (f *FS) ApplyTar(r io.Reader, b *Budget) error {
	tr := tar.NewReader(r)
	var (
		opaque    [][]string
		whiteouts [][]string
		adds      []staged
	)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return layerErr("tar: %v", err)
		}
		switch hdr.Typeflag {
		case tar.TypeXGlobalHeader, tar.TypeXHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			continue
		}
		comps, err := entryName(hdr.Name, b.Limits)
		if err != nil {
			return err
		}
		if len(comps) == 0 {
			continue // the root itself
		}
		for _, c := range comps[:len(comps)-1] {
			if strings.HasPrefix(c, whiteoutPrefix) {
				return layerErr("entry %q lies under a whiteout name", hdr.Name)
			}
		}
		base := comps[len(comps)-1]
		if strings.HasPrefix(base, whiteoutPrefix) {
			if hdr.Typeflag == tar.TypeDir {
				return layerErr("whiteout %q is a directory", hdr.Name)
			}
			if base == opaqueMarker {
				opaque = append(opaque, comps[:len(comps)-1])
				continue
			}
			victim := strings.TrimPrefix(base, whiteoutPrefix)
			if victim == "" || victim == "." || victim == ".." || strings.HasPrefix(victim, whiteoutPrefix) {
				return layerErr("invalid whiteout %q", hdr.Name)
			}
			whiteouts = append(whiteouts, append(append([]string{}, comps[:len(comps)-1]...), victim))
			continue
		}
		if err := b.spend(0, 1); err != nil {
			return err
		}
		s := staged{hdr: hdr, comps: comps}
		switch hdr.Typeflag {
		case tar.TypeReg, 0, '7': // 0: pre-POSIX regular file; '7': contiguous file
			if hdr.Size < 0 {
				return layerErr("negative size for %q", hdr.Name)
			}
			if err := b.spend(hdr.Size, 0); err != nil {
				return err
			}
			s.data = make([]byte, hdr.Size)
			if _, err := io.ReadFull(tr, s.data); err != nil {
				return layerErr("reading %q: %v", hdr.Name, err)
			}
		case tar.TypeSymlink:
			if err := checkSymlinkTarget(comps[:len(comps)-1], hdr.Linkname, b.Limits); err != nil {
				return err
			}
			if err := b.spend(int64(len(hdr.Linkname)), 0); err != nil {
				return err
			}
		case tar.TypeLink:
			lc, err := entryName(hdr.Linkname, b.Limits)
			if err != nil {
				return layerErr("hard link %q: %v", hdr.Name, err)
			}
			if len(lc) == 0 {
				return layerErr("hard link %q to the root", hdr.Name)
			}
			s.link = lc
		case tar.TypeDir:
		default:
			// Devices, FIFOs and anything newer mean nothing to a wasm
			// guest; they are skipped, not created.
			continue
		}
		adds = append(adds, s)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	for _, dir := range opaque {
		n, err := f.lookupNoFollowLocked(dir)
		if err != nil {
			return err
		}
		if n != nil && n.isDir() {
			d, err := f.mutDirLocked(dir)
			if err != nil {
				return err
			}
			clear(d.kids)
		}
	}
	for _, w := range whiteouts {
		n, err := f.lookupNoFollowLocked(w)
		if err != nil {
			return err
		}
		if n == nil {
			continue
		}
		d, err := f.mutDirLocked(w[:len(w)-1])
		if err != nil {
			return err
		}
		delete(d.kids, w[len(w)-1])
	}
	for i := range adds {
		if err := f.addLocked(&adds[i]); err != nil {
			return err
		}
	}
	return nil
}

// lookupNoFollowLocked finds comps without following any symlink. It
// refuses a path whose parents are not all real directories.
func (f *FS) lookupNoFollowLocked(comps []string) (*node, error) {
	cur := f.root
	for i, c := range comps {
		if !cur.isDir() {
			return nil, layerErr("%q lies under a non-directory", strings.Join(comps, "/"))
		}
		next := cur.kids[c]
		if next == nil {
			return nil, nil
		}
		if i < len(comps)-1 && !next.isDir() {
			return nil, layerErr("%q lies under a non-directory", strings.Join(comps, "/"))
		}
		cur = next
	}
	return cur, nil
}

// mkParentsLocked makes every parent of comps a real directory, creating
// missing ones 0755, and returns the owned parent.
func (f *FS) mkParentsLocked(comps []string) (*node, error) {
	dir, err := f.mutDirLocked(nil)
	if err != nil {
		return nil, err
	}
	for _, c := range comps[:len(comps)-1] {
		next := dir.kids[c]
		switch {
		case next == nil:
			next = f.newNode(fs.ModeDir | 0o755)
			dir.kids[c] = next
		case !next.isDir():
			return nil, layerErr("%q lies under a non-directory (%s)", strings.Join(comps, "/"), c)
		}
		dir = f.mutChildLocked(dir, c)
	}
	return dir, nil
}

func (f *FS) addLocked(s *staged) error {
	dir, err := f.mkParentsLocked(s.comps)
	if err != nil {
		return err
	}
	name := s.comps[len(s.comps)-1]
	mtime := s.hdr.ModTime
	if mtime.IsZero() {
		mtime = time.Unix(0, 0)
	}
	perm := fs.FileMode(s.hdr.Mode).Perm()
	switch s.hdr.Typeflag {
	case tar.TypeDir:
		if old := dir.kids[name]; old != nil && old.isDir() {
			n := f.mutChildLocked(dir, name)
			n.mode = fs.ModeDir | perm
			n.mtime = mtime
			return nil
		}
		n := f.newNode(fs.ModeDir | perm)
		n.mtime = mtime
		dir.kids[name] = n
	case tar.TypeSymlink:
		n := f.newNode(fs.ModeSymlink | 0o777)
		n.target = s.hdr.Linkname
		n.mtime = mtime
		dir.kids[name] = n
	case tar.TypeLink:
		target, err := f.lookupNoFollowLocked(s.link)
		if err != nil {
			return err
		}
		if target == nil || !target.isRegular() {
			return layerErr("hard link %q to %q, which is not an existing regular file", s.hdr.Name, path.Join(s.link...))
		}
		// Own the target so both names share one node in this tree.
		tdir, err := f.mutDirLocked(s.link[:len(s.link)-1])
		if err != nil {
			return err
		}
		target = f.mutChildLocked(tdir, s.link[len(s.link)-1])
		if dir.kids[name] == target {
			return nil
		}
		target.nlink++
		dir.kids[name] = target
	default:
		n := f.newNode(perm)
		n.data = s.data
		n.mtime = mtime
		dir.kids[name] = n
	}
	return nil
}

// LayerReader is one layer to build from.
type LayerReader struct {
	MediaType string
	Title     string
	Open      func() (io.ReadCloser, error)
}

// Build applies layers in order to a new tree and freezes it.
func Build(layers []LayerReader, lim Limits) (*FS, error) {
	f := New()
	b := NewBudget(lim)
	for i, l := range layers {
		rc, err := l.Open()
		if err != nil {
			return nil, fmt.Errorf("layer %d: %w", i, err)
		}
		err = f.ApplyLayer(rc, l.MediaType, l.Title, b)
		rc.Close()
		if err != nil {
			return nil, fmt.Errorf("layer %d: %w", i, err)
		}
	}
	f.Freeze()
	return f, nil
}
