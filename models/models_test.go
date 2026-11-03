package models

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
)

type memSource struct {
	b     []byte
	reads int
}

func (m *memSource) ReadAt(p []byte, off int64) (int, error) {
	m.reads++
	if off >= int64(len(m.b)) {
		return 0, io.EOF
	}
	n := copy(p, m.b[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (m *memSource) Size() int64  { return int64(len(m.b)) }
func (m *memSource) Close() error { return nil }

func entryFor(b []byte) Entry {
	s := sha256.Sum256(b)
	return Entry{Name: "tiny", Repo: "o/r", Revision: "0123456789abcdef0123456789abcdef01234567", File: "tiny.gguf",
		Size: int64(len(b)), SHA256: hex.EncodeToString(s[:])}
}

func TestRegistryEntriesAreWellFormed(t *testing.T) {
	for _, e := range Registry {
		if err := e.Check(); err != nil {
			t.Errorf("%v", err)
		}
	}
	if _, err := Lookup("nope"); !errors.Is(err, ErrUnknown) {
		t.Fatal(err)
	}
}

func TestCheckRefusals(t *testing.T) {
	good := entryFor([]byte("x"))
	for name, mut := range map[string]func(*Entry){
		"slash in name": func(e *Entry) { e.Name = "a/b" },
		"comma in name": func(e *Entry) { e.Name = "a,b" },
		"branch":        func(e *Entry) { e.Revision = "main" },
		"short sha":     func(e *Entry) { e.SHA256 = "ab" },
		"upper sha":     func(e *Entry) { e.SHA256 = bytes.NewBufferString(e.SHA256).String()[:63] + "G" },
		"huge":          func(e *Entry) { e.Size = MaxSize + 1 },
		"dotdot file":   func(e *Entry) { e.File = "../x" },
	} {
		e := good
		mut(&e)
		if e.Check() == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestVerify(t *testing.T) {
	data := bytes.Repeat([]byte("model"), 1000)
	e := entryFor(data)
	if err := Verify(&memSource{b: data}, e); err != nil {
		t.Fatal(err)
	}
	// Wrong size: refused without reading a byte.
	s := &memSource{b: data[:len(data)-1]}
	if err := Verify(s, e); !errors.Is(err, ErrSize) || s.reads != 0 {
		t.Fatalf("%v, %d reads", err, s.reads)
	}
	// Same size, one flipped bit.
	bad := bytes.Clone(data)
	bad[100] ^= 1
	if err := Verify(&memSource{b: bad}, e); !errors.Is(err, ErrChecksum) {
		t.Fatal(err)
	}
}

func TestMountIsReadOnlyAndRangeReadable(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 100)
	e := entryFor(data)
	src := &memSource{b: data}
	fsys, err := Mount(map[string]Source{"tiny": src}, []Entry{e})
	if err != nil {
		t.Fatal(err)
	}
	st, err := fsys.Stat("/tiny.gguf")
	if err != nil || st.Size() != int64(len(data)) {
		t.Fatalf("%v %v", st, err)
	}
	f, err := fsys.OpenFile("/tiny.gguf", rootfs.O_RDONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	p := make([]byte, 10)
	if n, err := f.ReadAt(p, 990); n != 10 || (err != nil && err != io.EOF) || string(p) != "0123456789" {
		t.Fatalf("%d %v %q", n, err, p)
	}
	if n, err := f.ReadAt(p, 995); n != 5 || err != io.EOF {
		t.Fatalf("short read: %d %v", n, err)
	}
	if _, err := f.Seek(-3, io.SeekEnd); err != nil {
		t.Fatal(err)
	}
	if n, err := f.Read(p); n != 3 || err != nil {
		t.Fatalf("read at end: %d %v", n, err)
	}
	if n, err := f.Read(p); n != 0 || err != io.EOF {
		t.Fatalf("read past end: %d %v", n, err)
	}
	if src.reads > 3 {
		t.Fatalf("the mount read the whole file: %d reads", src.reads)
	}
	// No write, truncate or open for writing, in the tree or in a clone.
	for _, fs := range []*rootfs.FS{fsys, fsys.Clone(rootfs.Quota{})} {
		if _, err := fs.OpenFile("/tiny.gguf", rootfs.O_WRONLY, 0); !errors.Is(err, rootfs.ErrReadOnly) {
			t.Errorf("open for writing: %v", err)
		}
		if _, err := fs.OpenFile("/tiny.gguf", rootfs.O_RDONLY|rootfs.O_TRUNC, 0); !errors.Is(err, rootfs.ErrReadOnly) {
			t.Errorf("truncating open: %v", err)
		}
		if err := fs.WriteFile("/tiny.gguf", []byte("x"), 0o644); err == nil {
			t.Error("WriteFile succeeded")
		}
		if err := fs.WriteFile("/other", []byte("x"), 0o644); err == nil && fs == fsys {
			t.Error("the frozen mount accepted a new file")
		}
	}
	if _, err := Mount(map[string]Source{}, []Entry{e}); err == nil {
		t.Fatal("a mount without a source")
	}
}

func TestOpenVerifiesOnceAndRefusesCorruption(t *testing.T) {
	dir := t.TempDir()
	data := bytes.Repeat([]byte("abc"), 5000)
	e := entryFor(data)
	if _, err := Open(dir, e); err == nil {
		t.Fatal("opened a missing file")
	}
	p := CachePath(dir, e)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Corrupt it in place (same size): detected, the memo is keyed by mtime.
	bad := bytes.Clone(data)
	bad[7] ^= 0xff
	if err := os.WriteFile(p, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Hour)
	os.Chtimes(p, later, later)
	if _, err := Open(dir, e); !errors.Is(err, ErrChecksum) {
		t.Fatalf("corrupted cache file: %v", err)
	}
}

func TestFetch(t *testing.T) {
	data := bytes.Repeat([]byte("weights"), 2000)
	e := entryFor(data)
	var cdnHits int
	cdn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cdnHits++
		w.Write(data)
	}))
	defer cdn.Close()
	cu, _ := url.Parse(cdn.URL)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			http.Redirect(w, r, cdn.URL+"/blob", http.StatusFound)
		case "/evil":
			http.Redirect(w, r, "http://127.0.0.2:1/x", http.StatusFound)
		case "/big":
			w.Write(append(bytes.Clone(data), 0))
		case "/short":
			w.Write(data[:10])
		case "/flip":
			b := bytes.Clone(data)
			b[0] ^= 1
			w.Write(b)
		}
	}))
	defer hub.Close()
	hu, _ := url.Parse(hub.URL)
	f := Fetcher{Hosts: []string{hu.Hostname(), cu.Hostname()}}
	ctx := context.Background()
	dir := t.TempDir()
	if err := (Fetcher{}).Fetch(ctx, dir, e, hub.URL+"/ok"); !errors.Is(err, ErrHost) {
		t.Fatalf("default hosts accepted a local server: %v", err)
	}
	// 127.0.0.2 is not in the allowed hosts: the redirect is refused.
	if err := f.Fetch(ctx, dir, e, hub.URL+"/evil"); !errors.Is(err, ErrHost) {
		t.Fatalf("redirect to another host: %v", err)
	}
	for _, p := range []string{"/big", "/short"} {
		if err := f.Fetch(ctx, dir, e, hub.URL+p); !errors.Is(err, ErrSize) {
			t.Errorf("%s: %v", p, err)
		}
	}
	if err := f.Fetch(ctx, dir, e, hub.URL+"/flip"); !errors.Is(err, ErrChecksum) {
		t.Errorf("flipped bit: %v", err)
	}
	if _, err := os.Stat(CachePath(dir, e)); err == nil {
		t.Fatal("a refused download left a cache file")
	}
	if err := f.Fetch(ctx, dir, e, hub.URL+"/ok"); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, e); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "pull-*")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}
