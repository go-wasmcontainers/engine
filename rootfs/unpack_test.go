package rootfs

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"io/fs"
	"sort"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/internal/testutil"
)

type E = testutil.TarEntry

func apply(t *testing.T, f *FS, b *Budget, entries ...E) error {
	t.Helper()
	return f.ApplyTar(bytes.NewReader(testutil.Tar(entries...)), b)
}

// tree lists every path of f, directories with a trailing slash.
func tree(f *FS) string {
	var out []string
	var walk func(p string)
	walk = func(p string) {
		ents, _ := f.ReadDir(p)
		for _, e := range ents {
			q := strings.TrimPrefix(p+"/"+e.Name(), "/")
			if e.IsDir() {
				out = append(out, q+"/")
				walk("/" + q)
			} else {
				out = append(out, q)
			}
		}
	}
	walk("")
	sort.Strings(out)
	return strings.Join(out, " ")
}

func TestWhiteoutsFromTheSpec(t *testing.T) {
	// layer.md, "Whiteouts": delete file1, a/file2 and b/, add file4.
	f := New()
	b := NewBudget(DefaultLimits)
	mustNil(t, apply(t, f, b,
		E{Name: "file1", Body: []byte("1")},
		E{Name: "a/file2", Body: []byte("2")},
		E{Name: "b/", Type: tar.TypeDir},
		E{Name: "c/file3", Body: []byte("3")},
	))
	mustNil(t, apply(t, f, b,
		E{Name: ".wh.file1"},
		E{Name: "a/.wh.file2"},
		E{Name: ".wh.b"},
		E{Name: "file4", Body: []byte("4")},
	))
	if got, want := tree(f), "a/ c/ c/file3 file4"; got != want {
		t.Fatalf("tree %q, want %q", got, want)
	}
}

func TestOpaqueWhiteoutIsAppliedFirst(t *testing.T) {
	// layer.md, "Opaque Whiteout": a/b is deleted and recreated with
	// a/b/c/foo; the marker may come after the new entries.
	for _, order := range []string{"before", "after"} {
		t.Run(order, func(t *testing.T) {
			f := New()
			b := NewBudget(DefaultLimits)
			mustNil(t, apply(t, f, b,
				E{Name: "a/", Type: tar.TypeDir},
				E{Name: "a/b/", Type: tar.TypeDir},
				E{Name: "a/b/c/", Type: tar.TypeDir},
				E{Name: "a/b/c/bar", Body: []byte("bar")},
			))
			opq := E{Name: "a/.wh..wh..opq"}
			layer := []E{{Name: "a/", Type: tar.TypeDir}, {Name: "a/b/", Type: tar.TypeDir},
				{Name: "a/b/c/", Type: tar.TypeDir}, {Name: "a/b/c/foo", Body: []byte("foo")}}
			if order == "before" {
				layer = append([]E{layer[0], opq}, layer[1:]...)
			} else {
				layer = append(layer, opq)
			}
			mustNil(t, apply(t, f, b, layer...))
			if got, want := tree(f), "a/ a/b/ a/b/c/ a/b/c/foo"; got != want {
				t.Fatalf("tree %q, want %q", got, want)
			}
		})
	}
}

func TestWhiteoutDoesNotHideItsOwnLayer(t *testing.T) {
	f := New()
	b := NewBudget(DefaultLimits)
	mustNil(t, apply(t, f, b, E{Name: "x", Body: []byte("old")}))
	// "Files that are present in the same layer as a whiteout file can
	// only be hidden by whiteout files in subsequent layers."
	mustNil(t, apply(t, f, b, E{Name: "x", Body: []byte("new")}, E{Name: ".wh.x"}, E{Name: ".wh.never"}))
	if read(t, f, "/x") != "new" {
		t.Fatal("a whiteout hid an entry of its own layer")
	}
	if tree(f) != "x" {
		t.Fatal("the whiteout itself is visible:", tree(f))
	}
}

func TestReplaceAndAttributes(t *testing.T) {
	f := New()
	b := NewBudget(DefaultLimits)
	mustNil(t, apply(t, f, b,
		E{Name: "./", Type: tar.TypeDir},
		E{Name: "./d/", Type: tar.TypeDir, Mode: 0o700},
		E{Name: "d/f", Body: []byte("f")},
		E{Name: "e", Body: []byte("file")},
		E{Name: "dev", Type: tar.TypeChar},
		E{Name: "fifo", Type: tar.TypeFifo},
	))
	// Dir over dir keeps the children and takes the new attributes; file
	// over dir and dir over file replace.
	mustNil(t, apply(t, f, b,
		E{Name: "d/", Type: tar.TypeDir, Mode: 0o755},
		E{Name: "e/", Type: tar.TypeDir},
	))
	st, _ := f.Stat("/d")
	if st.Mode().Perm() != 0o755 || read(t, f, "/d/f") != "f" {
		t.Fatal("dir over dir")
	}
	if st, _ := f.Stat("/e"); !st.IsDir() {
		t.Fatal("dir over file")
	}
	if _, err := f.Stat("/dev"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a device node was created")
	}
	mustNil(t, apply(t, f, b, E{Name: "d", Body: []byte("now a file")}))
	if read(t, f, "/d") != "now a file" {
		t.Fatal("file over dir")
	}
}

func TestLinks(t *testing.T) {
	f := New()
	b := NewBudget(DefaultLimits)
	mustNil(t, apply(t, f, b,
		E{Name: "usr/bin/tool", Body: []byte("T"), Mode: 0o755},
		E{Name: "bin/tool", Type: tar.TypeLink, Link: "usr/bin/tool"},
		E{Name: "bin/rel", Type: tar.TypeSymlink, Link: "../usr/bin/tool"},
		E{Name: "bin/abs", Type: tar.TypeSymlink, Link: "/usr/bin/tool"},
	))
	mustNil(t, apply(t, f, b, E{Name: "bin/again", Type: tar.TypeLink, Link: "./usr/bin/tool"}))
	for _, p := range []string{"/bin/tool", "/bin/rel", "/bin/abs", "/bin/again"} {
		if read(t, f, p) != "T" {
			t.Fatal(p)
		}
	}
	st, _ := f.Stat("/usr/bin/tool")
	if st.Nlink != 3 {
		t.Fatalf("nlink %d", st.Nlink)
	}
}

func TestRefusals(t *testing.T) {
	cases := []struct {
		name    string
		lower   []E
		entries []E
	}{
		{"absolute", nil, []E{{Name: "/etc/passwd", Body: []byte("x")}}},
		{"dotdot", nil, []E{{Name: "a/../../etc/passwd", Body: []byte("x")}}},
		{"symlink escape", nil, []E{{Name: "a/l", Type: tar.TypeSymlink, Link: "../../etc"}}},
		{"symlink empty", nil, []E{{Name: "l", Type: tar.TypeSymlink, Link: ""}}},
		{"write through symlink, same layer", nil, []E{
			{Name: "l", Type: tar.TypeSymlink, Link: "/tmp"},
			{Name: "l/x", Body: []byte("x")},
		}},
		{"write through symlink, lower layer", []E{{Name: "l", Type: tar.TypeSymlink, Link: "/tmp"}},
			[]E{{Name: "l/x", Body: []byte("x")}}},
		{"write under a file", []E{{Name: "f", Body: []byte("x")}}, []E{{Name: "f/x", Body: []byte("x")}}},
		{"whiteout through symlink", []E{{Name: "d/", Type: tar.TypeDir}, {Name: "l", Type: tar.TypeSymlink, Link: "d"}},
			[]E{{Name: "l/.wh.x"}}},
		{"opaque under symlink", []E{{Name: "d/", Type: tar.TypeDir}, {Name: "l", Type: tar.TypeSymlink, Link: "d"}},
			[]E{{Name: "l/x/.wh..wh..opq"}}},
		{"hardlink to missing", nil, []E{{Name: "h", Type: tar.TypeLink, Link: "nope"}}},
		{"hardlink to dir", []E{{Name: "d/", Type: tar.TypeDir}}, []E{{Name: "h", Type: tar.TypeLink, Link: "d"}}},
		{"hardlink escape", nil, []E{{Name: "h", Type: tar.TypeLink, Link: "../etc/passwd"}}},
		{"hardlink absolute", nil, []E{{Name: "h", Type: tar.TypeLink, Link: "/etc/passwd"}}},
		{"hardlink to root", nil, []E{{Name: "h", Type: tar.TypeLink, Link: "./"}}},
		{"hardlink to symlink", []E{{Name: "s", Type: tar.TypeSymlink, Link: "x"}}, []E{{Name: "h", Type: tar.TypeLink, Link: "s"}}},
		{"whiteout dir", nil, []E{{Name: ".wh.x/", Type: tar.TypeDir}}},
		{"under a whiteout name", nil, []E{{Name: ".wh.x/y", Body: []byte("y")}}},
		{"empty whiteout", nil, []E{{Name: ".wh."}}},
		{"double whiteout", nil, []E{{Name: ".wh..wh.x"}}},
		{"too deep", nil, []E{{Name: strings.Repeat("d/", 200) + "f", Body: []byte("x")}}},
		{"long component", nil, []E{{Name: strings.Repeat("n", 256), Body: []byte("x")}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := New()
			b := NewBudget(DefaultLimits)
			if c.lower != nil {
				mustNil(t, apply(t, f, b, c.lower...))
			}
			err := apply(t, f, b, c.entries...)
			if !errors.Is(err, ErrLayer) {
				t.Fatalf("got %v, want ErrLayer", err)
			}
		})
	}
	// An opaque marker in a symlink "directory" is not followed: the
	// directory the link points at keeps its children.
	{
		f := New()
		b := NewBudget(DefaultLimits)
		mustNil(t, apply(t, f, b, E{Name: "d/keep", Body: []byte("k")}, E{Name: "l", Type: tar.TypeSymlink, Link: "d"}))
		mustNil(t, apply(t, f, b, E{Name: "l/.wh..wh..opq"}))
		if read(t, f, "/d/keep") != "k" {
			t.Fatal("an opaque whiteout followed a symlink")
		}
	}
	// archive/tar cannot even write a NUL into a name; refuse one read.
	if _, err := entryName("a\x00b", DefaultLimits); !errors.Is(err, ErrLayer) {
		t.Fatal(err)
	}
	if err := checkSymlinkTarget(nil, "a\x00b", DefaultLimits); !errors.Is(err, ErrLayer) {
		t.Fatal(err)
	}
	// A frozen tree refuses a layer.
	f := New()
	f.Freeze()
	if err := apply(t, f, NewBudget(DefaultLimits), E{Name: "x", Body: []byte("x")}); !errors.Is(err, ErrReadOnly) {
		t.Fatal(err)
	}
	// Garbage is not a tar.
	if err := New().ApplyTar(strings.NewReader(strings.Repeat("garbage!", 100)), NewBudget(DefaultLimits)); !errors.Is(err, ErrLayer) {
		t.Fatal(err)
	}
}

func TestBudget(t *testing.T) {
	lim := DefaultLimits
	lim.MaxBytes = 100
	lim.MaxFiles = 3
	f := New()
	b := NewBudget(lim)
	mustNil(t, apply(t, f, b, E{Name: "a", Body: make([]byte, 60)}))
	if err := apply(t, f, b, E{Name: "b", Body: make([]byte, 60)}); !errors.Is(err, ErrLayer) {
		t.Fatal("byte budget not enforced across layers:", err)
	}
	b = NewBudget(lim)
	if err := apply(t, New(), b, E{Name: "1"}, E{Name: "2"}, E{Name: "3"}, E{Name: "4"}); !errors.Is(err, ErrLayer) {
		t.Fatal("file budget not enforced:", err)
	}
	lim.MaxBytes = 10
	if err := apply(t, New(), NewBudget(lim), E{Name: "l", Type: tar.TypeSymlink, Link: strings.Repeat("x", 20)}); !errors.Is(err, ErrLayer) {
		t.Fatal("symlink target not charged:", err)
	}
	lim.MaxPathLen = 10
	if err := apply(t, New(), NewBudget(lim), E{Name: strings.Repeat("p", 11)}); !errors.Is(err, ErrLayer) {
		t.Fatal("path length not enforced:", err)
	}
}

func TestDecompressionBomb(t *testing.T) {
	// 64 MiB of zeros compresses to about 64 KiB: a 1000:1 ratio.
	bomb := testutil.Gzip(testutil.Tar(E{Name: "zeros", Body: make([]byte, 64<<20)}))
	err := New().ApplyLayer(bytes.NewReader(bomb), mtTarGz, "", NewBudget(DefaultLimits))
	if !errors.Is(err, ErrLayer) || !strings.Contains(err.Error(), "ratio") {
		t.Fatalf("got %v, want a ratio refusal", err)
	}
	// The same content within an honest ratio is accepted.
	lim := DefaultLimits
	lim.MaxRatio = 5000
	if err := New().ApplyLayer(bytes.NewReader(bomb), mtTarGz, "", NewBudget(lim)); err != nil {
		t.Fatal(err)
	}
	if err := New().ApplyLayer(strings.NewReader("not gzip"), mtTarGz, "", NewBudget(DefaultLimits)); !errors.Is(err, ErrLayer) {
		t.Fatal(err)
	}
	if err := New().ApplyLayer(strings.NewReader(""), "application/x-unknown", "", NewBudget(DefaultLimits)); !errors.Is(err, ErrLayer) {
		t.Fatal(err)
	}
}

func TestWasmLayer(t *testing.T) {
	mod := []byte("\x00asm\x01\x00\x00\x00")
	f := New()
	p, err := f.ApplyWasm(bytes.NewReader(mod), "hello.wasm", NewBudget(DefaultLimits))
	mustNil(t, err)
	if p != "/hello.wasm" || read(t, f, p) != string(mod) {
		t.Fatal(p)
	}
	for _, bad := range []string{"", "../x", "a/b", ".wh.x", "..", "."} {
		if ModulePath(bad) != "/module.wasm" {
			t.Fatalf("title %q not sanitised", bad)
		}
	}
	if err := New().ApplyLayer(strings.NewReader("MZ not wasm"), mtWasm, "", NewBudget(DefaultLimits)); !errors.Is(err, ErrLayer) {
		t.Fatal(err)
	}
	lim := DefaultLimits
	lim.MaxBytes = 4
	if _, err := New().ApplyWasm(bytes.NewReader(mod), "", NewBudget(lim)); !errors.Is(err, ErrLayer) {
		t.Fatal(err)
	}
}

func TestBuild(t *testing.T) {
	layers := []LayerReader{
		{MediaType: mtTarGz, Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(testutil.Gzip(testutil.Tar(E{Name: "a", Body: []byte("1")})))), nil
		}},
		{MediaType: mtTar, Open: func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(testutil.Tar(E{Name: ".wh.a"}, E{Name: "b", Body: []byte("2")}))), nil
		}},
	}
	f, err := Build(layers, DefaultLimits)
	mustNil(t, err)
	if tree(f) != "b" {
		t.Fatal(tree(f))
	}
	if err := f.WriteFile("/c", nil, 0o644); !errors.Is(err, ErrReadOnly) {
		t.Fatal("Build did not freeze")
	}
	_, err = Build([]LayerReader{{MediaType: mtTar, Open: func() (io.ReadCloser, error) { return nil, io.ErrUnexpectedEOF }}}, DefaultLimits)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	_, err = Build([]LayerReader{{MediaType: mtTar, Open: func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(testutil.Tar(E{Name: "/abs"}))), nil
	}}}, DefaultLimits)
	if !errors.Is(err, ErrLayer) {
		t.Fatal(err)
	}
}
