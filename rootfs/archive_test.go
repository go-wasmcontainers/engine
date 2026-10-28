package rootfs

import (
	"archive/tar"
	"bytes"
	"errors"
	"strings"
	"testing"
)

type tEntry struct {
	name, body, link string
	typ              byte
}

func tarOf(t *testing.T, es ...tEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range es {
		h := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: 0o644, Size: int64(len(e.body)), Linkname: e.link}
		if e.typ == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag == tar.TypeDir {
			h.Mode = 0o755
		}
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(e.body))
	}
	tw.Close()
	return buf.Bytes()
}

func TestExtractAndWriteTar(t *testing.T) {
	base := diffBase(t)
	c := base.Clone(Quota{MaxBytes: 1 << 20, MaxFiles: 100})
	in := tarOf(t,
		tEntry{name: "app/", typ: tar.TypeDir},
		tEntry{name: "app/main.wasm", body: "\x00asm"},
		tEntry{name: "app/conf/x.toml", body: "a = 1"},
		tEntry{name: "app/link", typ: tar.TypeSymlink, link: "main.wasm"},
		tEntry{name: "app/hard", typ: tar.TypeLink, link: "app/main.wasm"},
	)
	if err := c.Extract("/srv", bytes.NewReader(in), DefaultLimits, false); err == nil {
		t.Fatal("extracted into a directory that does not exist")
	}
	if err := c.Extract("/etc", bytes.NewReader(in), DefaultLimits, false); err != nil {
		t.Fatal(err)
	}
	if s := read(t, c, "/etc/app/conf/x.toml"); s != "a = 1" {
		t.Fatalf("%q", s)
	}
	if s := read(t, c, "/etc/app/link"); s != "\x00asm" {
		t.Fatalf("symlink: %q", s)
	}
	if s := read(t, c, "/etc/app/hard"); s != "\x00asm" {
		t.Fatalf("hard link: %q", s)
	}
	// Out and back: GET of /etc/app is a tar rooted at "app".
	var out bytes.Buffer
	if err := c.WriteTar("/etc/app", &out); err != nil {
		t.Fatal(err)
	}
	var names []string
	tr := tar.NewReader(&out)
	for {
		h, err := tr.Next()
		if err != nil {
			break
		}
		names = append(names, h.Name)
	}
	if got := strings.Join(names, " "); got != "app/ app/conf/ app/conf/x.toml app/hard app/link app/main.wasm" {
		t.Fatalf("tar: %s", got)
	}
	var one bytes.Buffer
	if err := c.WriteTar("/etc/motd", &one); err != nil {
		t.Fatal(err)
	}
	if h, err := tar.NewReader(&one).Next(); err != nil || h.Name != "motd" {
		t.Fatalf("single file: %v %v", h, err)
	}

	// Hostile archives.
	for name, bad := range map[string][]byte{
		"dotdot":    tarOf(t, tEntry{name: "../../escape", body: "x"}),
		"absolute":  tarOf(t, tEntry{name: "/etc/passwd", body: "x"}),
		"hard out":  tarOf(t, tEntry{name: "h", typ: tar.TypeLink, link: "../etc/motd"}),
		"device":    tarOf(t, tEntry{name: "dev", typ: tar.TypeChar}),
		"not clean": tarOf(t, tEntry{name: "a//b", body: "x"}),
		"too deep":  tarOf(t, tEntry{name: strings.Repeat("d/", 300) + "f", body: "x"}),
		"truncated": tarOf(t, tEntry{name: "f", body: strings.Repeat("x", 2000)})[:1000],
	} {
		if err := c.Extract("/etc", bytes.NewReader(bad), DefaultLimits, false); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A symlink in the container cannot lead a write out of the root: it
	// resolves inside it.
	c.Symlink("/../../..", "/etc/up")
	if err := c.Extract("/etc", bytes.NewReader(tarOf(t, tEntry{name: "up/x", body: "inside"})), DefaultLimits, false); err != nil {
		t.Fatal(err)
	}
	if s := read(t, c, "/x"); s != "inside" {
		t.Fatalf("through a climbing symlink: %q", s)
	}
	// The container's quota applies.
	small := base.Clone(Quota{MaxBytes: 10, MaxFiles: 100})
	if err := small.Extract("/", bytes.NewReader(tarOf(t, tEntry{name: "big", body: strings.Repeat("x", 100)})), DefaultLimits, false); err == nil {
		t.Fatal("quota not applied")
	}
	// noOverwriteDirNonDir.
	if err := c.Extract("/", bytes.NewReader(tarOf(t, tEntry{name: "etc", body: "x"})), DefaultLimits, true); !errors.Is(err, ErrArchive) {
		t.Fatalf("dir replaced by a file: %v", err)
	}
}

func FuzzExtract(f *testing.F) {
	f.Add(tarOfF(f, "a/b", "x"))
	f.Add(tarOfF(f, "../x", "x"))
	f.Fuzz(func(t *testing.T, b []byte) {
		c := New().Clone(Quota{MaxBytes: 1 << 16, MaxFiles: 64})
		lim := Limits{MaxBytes: 1 << 16, MaxFiles: 64, MaxPathLen: 256, MaxDepth: 16}
		c.Extract("/", bytes.NewReader(b), lim, false)
		// Whatever happened, the tree is still within its quota.
		if n, files := c.Usage(); n > 1<<16 || files > 64 {
			t.Fatalf("usage %d bytes %d files", n, files)
		}
	})
}

func tarOfF(f *testing.F, name, body string) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
	tw.Write([]byte(body))
	tw.Close()
	return buf.Bytes()
}
