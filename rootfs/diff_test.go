package rootfs

import (
	"archive/tar"
	"bytes"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"testing"
	"time"
)

// dump lists every entry of f with what makes it what it is.
func dump(t testing.TB, f *FS) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var lines []string
	ids := map[*node]string{}
	var walk func(p string, n *node)
	walk = func(p string, n *node) {
		line := fmt.Sprintf("%s %v %d", p, n.mode, n.mtime.UnixNano())
		switch {
		case n.isDir():
			for _, k := range sortedNames(n) {
				walk(p+"/"+k, n.kids[k])
			}
		case n.isSymlink():
			line += " -> " + n.target
		default:
			line += fmt.Sprintf(" %q", n.data)
			if n.nlink > 1 {
				if first, ok := ids[n]; ok {
					line += " link " + first
				} else {
					ids[n] = p
				}
			}
		}
		lines = append(lines, line)
	}
	walk("", f.root)
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func diffBase(t testing.TB) *FS {
	t.Helper()
	b := New()
	for p, c := range map[string]string{"etc/motd": "hello\n", "bin/x": "\x00asm", "d/a": "a", "d/b": "b", "keep/k": "k", "e/f/g": "deep"} {
		if err := b.MkdirAll(p[:strings.LastIndexByte(p, '/')], 0o755); err != nil {
			t.Fatal(err)
		}
		if err := b.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	b.Freeze()
	return b
}

func TestDiffRoundTrip(t *testing.T) {
	base := diffBase(t)
	q := Quota{MaxBytes: 1 << 20, MaxFiles: 100}
	c := base.Clone(q)
	var buf bytes.Buffer
	if _, err := c.WriteDiff(&buf); err != nil {
		t.Fatal(err)
	}
	if buf.Len() != 1024 { // an unchanged clone: the end-of-archive blocks only
		t.Fatalf("unchanged clone wrote %d bytes", buf.Len())
	}
	before := c.Changes()
	steps := []func() error{
		func() error { return c.WriteFile("new.txt", []byte("new"), 0o600) },
		func() error { return c.WriteFile("etc/motd", []byte("changed\n"), 0o644) },
		func() error { return c.Remove("bin/x") },
		func() error { return c.Remove("d/a") },
		func() error { return c.Remove("d/b") },
		func() error { return c.Rmdir("d") },
		func() error { return c.Mkdir("d", 0o700) }, // made again: d/a, d/b must not come back
		func() error { return c.WriteFile("d/c", []byte("c"), 0o644) },
		func() error { return c.Rename("keep", "moved") },
		func() error { return c.Symlink("../../../etc/motd", "e/up") }, // climbs: fine at run time
		func() error { return c.WriteFile(".wh.trick", []byte("not a whiteout"), 0o644) },
		func() error { return c.Mkdir(".wh..wh..opq", 0o755) },
		func() error { return c.Link("new.txt", "hard") },
		func() error { return c.Chmod("e/f/g", 0o400) },
		func() error { return c.Chtimes("e", time.Unix(12345, 678), true) },
	}
	for i, s := range steps {
		if err := s(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if c.Changes() == before {
		t.Fatal("Changes did not move")
	}
	buf.Reset()
	u, err := c.WriteDiff(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if b, n := c.Usage(); u.Bytes != b || u.Files != n {
		t.Fatalf("usage %+v, tree says %d %d", u, b, n)
	}
	r := base.Clone(q)
	if err := r.ApplyDiff(bytes.NewReader(buf.Bytes()), u, DefaultLimits); err != nil {
		t.Fatal(err)
	}
	if got, want := dump(t, r), dump(t, c); got != want {
		t.Fatalf("restored tree differs:\n%s\n--- want\n%s", got, want)
	}
	if b, n := r.Usage(); b != u.Bytes || n != u.Files {
		t.Fatalf("restored usage %d %d, want %+v", b, n, u)
	}
	// The restored tree is a working clone: it diffs the same way again.
	var again bytes.Buffer
	if _, err := r.WriteDiff(&again); err != nil {
		t.Fatal(err)
	}
	r2 := base.Clone(q)
	if err := r2.ApplyDiff(&again, u, DefaultLimits); err != nil {
		t.Fatal(err)
	}
	if dump(t, r2) != dump(t, c) {
		t.Fatal("second round trip differs")
	}
	// Hard links survive: writing one name shows through the other.
	if h, err := r.OpenFile("hard", O_WRONLY|O_APPEND, 0); err != nil {
		t.Fatal(err)
	} else {
		h.Write([]byte("+"))
		h.Close()
	}
	if b, _ := r.ReadFile("new.txt"); string(b) != "new+" {
		t.Fatalf("hard link lost: %q", b)
	}
	// The change list, as `docker diff` prints it.
	var got []string
	for _, ch := range c.Diff() {
		got = append(got, fmt.Sprintf("%c %s", "CAD"[ch.Kind], ch.Path))
	}
	want := "A /.wh..wh..opq,A /.wh.trick,C /bin,D /bin/x,C /d,A /d/c,C /e,C /e/f,C /e/f/g,A /e/up,C /etc,C /etc/motd,A /hard,D /keep,A /moved,A /moved/k,A /new.txt"
	if strings.Join(got, ",") != want {
		t.Fatalf("Diff:\n%s\nwant\n%s", strings.Join(got, ","), want)
	}
	if len(base.Clone(q).Diff()) != 0 {
		t.Fatal("an unchanged clone has changes")
	}
	// The base never changed.
	if b, _ := base.ReadFile("d/a"); string(b) != "a" {
		t.Fatal("base changed")
	}
}

func TestDiffWholeTreeAndUsageBound(t *testing.T) {
	f := New()
	f.WriteFile("a", []byte("abc"), 0o644)
	var buf bytes.Buffer
	if _, err := f.WriteDiff(&buf); err != nil {
		t.Fatal(err)
	}
	r := New().Clone(Quota{MaxBytes: 10, MaxFiles: 2})
	// A recorded usage is untrusted: it is bounded by the quota.
	if err := r.ApplyDiff(&buf, Usage{Bytes: 1 << 40, Files: -5}, DefaultLimits); err != nil {
		t.Fatal(err)
	}
	if b, n := r.Usage(); b != 10 || n != 0 {
		t.Fatalf("usage %d %d", b, n)
	}
	if b, _ := r.ReadFile("a"); string(b) != "abc" {
		t.Fatal(string(b))
	}
}

func TestApplyDiffRefuses(t *testing.T) {
	for name, entries := range map[string][]tarEntry{
		"opaque on a file":   {{name: "x", typ: '0', pax: map[string]string{paxWhiteout: "opaque"}}},
		"whiteout dir":       {{name: "x", typ: '5', pax: map[string]string{paxWhiteout: "file"}}},
		"unknown record":     {{name: "x", typ: '0', pax: map[string]string{paxWhiteout: "maybe"}}},
		"climbing name":      {{name: "../x", typ: '0'}},
		"absolute name":      {{name: "/x", typ: '0'}},
		"empty link target":  {{name: "x", typ: '2'}},
		"hard link to none":  {{name: "x", typ: '1', link: "nothing"}},
		"under a file":       {{name: "f", typ: '0', body: "x"}, {name: "f/g", typ: '0'}},
		"over budget (size)": {{name: "big", typ: '0', body: strings.Repeat("x", 2048)}},
	} {
		t.Run(name, func(t *testing.T) {
			r := diffBase(t).Clone(Quota{})
			lim := DefaultLimits
			lim.MaxBytes = 1024
			if err := r.ApplyDiff(bytes.NewReader(makeTar(t, entries)), Usage{}, lim); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// BenchmarkWriteDiff: a container that wrote 8 MiB in 64 files, the cost
// of one checkpoint of its layer.
func BenchmarkWriteDiff(b *testing.B) {
	c := diffBase(b).Clone(Quota{})
	c.MkdirAll("data", 0o755)
	for i := 0; i < 64; i++ {
		c.WriteFile(fmt.Sprintf("data/f%02d", i), bytes.Repeat([]byte{byte(i)}, 128<<10), 0o644)
	}
	var buf bytes.Buffer
	b.SetBytes(8 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf.Reset()
		if _, err := c.WriteDiff(&buf); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkApplyDiff: restoring that layer.
func BenchmarkApplyDiff(b *testing.B) {
	base := diffBase(b)
	c := base.Clone(Quota{})
	c.MkdirAll("data", 0o755)
	for i := 0; i < 64; i++ {
		c.WriteFile(fmt.Sprintf("data/f%02d", i), bytes.Repeat([]byte{byte(i)}, 128<<10), 0o644)
	}
	var buf bytes.Buffer
	u, _ := c.WriteDiff(&buf)
	b.SetBytes(8 << 20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := base.Clone(Quota{}).ApplyDiff(bytes.NewReader(buf.Bytes()), u, DefaultLimits); err != nil {
			b.Fatal(err)
		}
	}
}

type tarEntry struct {
	name, link, body string
	typ              byte
	pax              map[string]string
}

func makeTar(t *testing.T, es []tarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Linkname: e.link, Size: int64(len(e.body)), Mode: 0o644, PAXRecords: e.pax, Format: tar.FormatPAX}
		if e.typ != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Size > 0 {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	return buf.Bytes()
}

// FuzzApplyDiff feeds arbitrary bytes to the diff reader, which reads back
// what storage returns; seeded with real diffs.
func FuzzApplyDiff(f *testing.F) {
	base := diffBase(f)
	c := base.Clone(Quota{})
	c.WriteFile("n", []byte("n"), 0o644)
	c.Remove("d/a")
	c.Symlink("../../x", "e/l")
	c.Link("n", "h")
	var buf bytes.Buffer
	c.WriteDiff(&buf)
	f.Add(buf.Bytes())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		r := base.Clone(Quota{MaxBytes: 1 << 20, MaxFiles: 1000})
		lim := DefaultLimits
		lim.MaxBytes, lim.MaxFiles = 1<<20, 1000
		if err := r.ApplyDiff(bytes.NewReader(data), Usage{}, lim); err != nil {
			return
		}
		// Whatever was accepted is a tree that works and diffs again.
		var out bytes.Buffer
		if _, err := r.WriteDiff(&out); err != nil {
			t.Fatal(err)
		}
		r.WalkCheck(t)
	})
}

// WalkCheck stats every entry through the public API.
func (f *FS) WalkCheck(t *testing.T) {
	err := fs.WalkDir(f.IOFS(), ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // dangling symlinks and the like
		}
		_, err = d.Info()
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}
