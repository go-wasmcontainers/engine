package rootfs

import (
	"archive/tar"
	"bytes"
	iofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/internal/testutil"
)

// fuzzSeeds are the inputs the committed corpus is made of.
func fuzzSeeds() [][]byte {
	return [][]byte{
		testutil.Tar(E{Name: "a/b", Body: []byte("x")}, E{Name: ".wh.c"}, E{Name: "d/.wh..wh..opq"}),
		testutil.Tar(E{Name: "l", Type: tar.TypeSymlink, Link: "../../x"}),
		testutil.Tar(E{Name: "l", Type: tar.TypeSymlink, Link: "/tmp"}, E{Name: "l/x", Body: []byte("x")}),
		testutil.Tar(E{Name: "f", Body: []byte("x")}, E{Name: "h", Type: tar.TypeLink, Link: "f"}),
		testutil.Tar(E{Name: "h", Type: tar.TypeLink, Link: "../f"}),
		testutil.Tar(E{Name: "/abs", Body: []byte("x")}),
		testutil.Gzip(testutil.Tar(E{Name: "z", Body: make([]byte, 4096)})),
		[]byte("not a tar at all"),
	}
}

// FuzzApplyTar feeds arbitrary bytes to the layer unpacker, both as a tar
// and as a gzip stream, over a lower layer that already holds a symlink
// and a file. Whatever happens, it must not panic, must stay within its
// budget, and must never leave a whiteout name or an escaping symlink in
// the tree.
func FuzzApplyTar(f *testing.F) {
	for _, s := range fuzzSeeds() {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		lim := DefaultLimits
		lim.MaxBytes = 1 << 20
		lim.MaxFiles = 256
		for _, mt := range []string{mtTar, mtTarGz} {
			fs := New()
			b := NewBudget(lim)
			if err := fs.ApplyTar(bytes.NewReader(testutil.Tar(
				E{Name: "etc/passwd", Body: []byte("root")},
				E{Name: "link", Type: tar.TypeSymlink, Link: "etc"},
			)), b); err != nil {
				t.Fatal(err)
			}
			err := fs.ApplyLayer(bytes.NewReader(data), mt, "", b)
			if b.Bytes > lim.MaxBytes || b.Files > lim.MaxFiles {
				t.Fatalf("budget overspent: %d bytes, %d files", b.Bytes, b.Files)
			}
			if err != nil {
				continue
			}
			checkTree(t, fs, nil)
		}
	})
}

func checkTree(t *testing.T, f *FS, dir []string) {
	p := "/" + strings.Join(dir, "/")
	ents, err := f.ReadDir(p)
	if err != nil {
		t.Fatalf("readdir %s: %v", p, err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), whiteoutPrefix) {
			t.Fatalf("whiteout name %q left in %s", e.Name(), p)
		}
		child := append(append([]string{}, dir...), e.Name())
		switch {
		case e.IsDir():
			checkTree(t, f, child)
		case e.Type()&iofs.ModeSymlink != 0:
			target, err := f.Readlink("/" + strings.Join(child, "/"))
			if err != nil {
				t.Fatal(err)
			}
			if err := checkSymlinkTarget(dir, target, DefaultLimits); err != nil {
				t.Fatalf("escaping symlink kept: %s -> %s", strings.Join(child, "/"), target)
			}
		}
	}
}

var resolveSeeds = []string{"/a/../../b", "loop", "x/y/z", "/abs/./..//c", strings.Repeat("../", 50), "/a/up/a/b/c"}

func TestWriteFuzzCorpus(t *testing.T) {
	seeds := []any{}
	for _, s := range fuzzSeeds() {
		seeds = append(seeds, s)
	}
	testutil.WriteCorpus(t, "FuzzApplyTar", seeds...)
	rs := []any{}
	for _, s := range resolveSeeds {
		rs = append(rs, s)
	}
	testutil.WriteCorpus(t, "FuzzResolve", rs...)
}

// TestCorpusIsCommitted fails when the committed corpus is missing, so a
// fuzz lane can never run on nothing but the in-code seeds by accident.
func TestCorpusIsCommitted(t *testing.T) {
	for _, n := range []string{"FuzzApplyTar", "FuzzResolve"} {
		ents, err := os.ReadDir(filepath.Join("testdata", "fuzz", n))
		if err != nil || len(ents) == 0 {
			t.Fatalf("no committed corpus for %s: %v", n, err)
		}
	}
}

// FuzzResolve throws arbitrary paths at the resolver of a tree full of
// symlinks: it must terminate and never panic.
func FuzzResolve(f *testing.F) {
	for _, s := range resolveSeeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, p string) {
		tr := New()
		tr.MkdirAll("/a/b", 0o755)
		tr.Symlink("loop", "/loop")
		tr.Symlink("/a", "/abs")
		tr.Symlink("../../..", "/a/up")
		tr.WriteFile("/a/b/c", []byte("c"), 0o644)
		tr.Stat(p)
		tr.Lstat(p)
		tr.ReadDir(p)
		if h, err := tr.OpenFile(p, O_RDWR|O_CREATE, 0o644); err == nil {
			h.Write([]byte("x"))
			h.Close()
		}
		tr.Rename(p, "/a/moved")
		tr.Remove(p)
		// The tree is still whole: the root is a directory.
		if st, err := tr.Stat("/"); err != nil || !st.IsDir() {
			t.Fatal("root lost")
		}
	})
}
