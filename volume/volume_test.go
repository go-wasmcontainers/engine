package volume

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/state"
)

var lim = rootfs.Limits{MaxBytes: 64 << 20, MaxFiles: 1 << 14, MaxRatio: 1, MaxPathLen: 4096, MaxDepth: 128}

func none(string) []string { return nil }

func TestVolumes(t *testing.T) {
	store := &state.Mem{}
	m := New(Options{Store: store})
	ctx := context.Background()
	v, created, err := m.Create("Data_1", map[string]string{"k": "v"})
	if err != nil || !created {
		t.Fatal(err)
	}
	if again, created, _ := m.Create("Data_1", nil); again != v || created {
		t.Fatal("create is not idempotent")
	}
	for _, bad := range []string{"/abs", "a", ".x", "a/b", strings.Repeat("x", 200)} {
		if _, _, err := m.Create(bad, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	anon, _, _ := m.Create("", nil)
	if !anon.Anonymous || len(anon.Name) != 64 {
		t.Fatal(anon.Name)
	}
	v.FS().MkdirAll("/sub", 0o755)
	v.FS().WriteFile("/sub/f", []byte("kept"), 0o644)
	if err := m.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// In use: refused.
	if err := m.Remove("Data_1", func(string) []string { return []string{"abc"} }); !errors.Is(err, ErrInUse) {
		t.Fatal(err)
	}
	// A restart restores names, labels and content.
	m2 := New(Options{Store: store})
	if rep, err := m2.Restore(lim); err != nil || len(rep) != 0 {
		t.Fatal(rep, err)
	}
	r, err := m2.Get("Data_1")
	if err != nil || r.Labels["k"] != "v" {
		t.Fatal(err)
	}
	if b, _ := r.FS().ReadFile("/sub/f"); string(b) != "kept" {
		t.Fatalf("%q", b)
	}
	if r.Size() != 4 {
		t.Fatalf("size %d", r.Size())
	}
	// Prune takes the anonymous one by default, every unused one with all.
	if gone, _ := m2.Prune(false, none); len(gone) != 1 || gone[0] != anon.Name {
		t.Fatal(gone)
	}
	if gone, n := m2.Prune(true, none); len(gone) != 1 || n != 4 {
		t.Fatal(gone, n)
	}
	m2.Flush(ctx)
	if ks, _ := store.Keys(""); len(ks) != 0 {
		t.Fatalf("left: %v", ks)
	}
	// The quota holds.
	q := New(Options{Quota: rootfs.Quota{MaxBytes: 4}})
	small, _, _ := q.Create("small", nil)
	if err := small.FS().WriteFile("/x", []byte("too big"), 0o644); !errors.Is(err, rootfs.ErrNoSpace) {
		t.Fatal(err)
	}
	m.Close(ctx)
	m2.Close(ctx)
}

func TestRestoreRepairs(t *testing.T) {
	store := &state.Mem{}
	m := New(Options{Store: store})
	good, _, _ := m.Create("good", nil)
	good.FS().WriteFile("/a", []byte("a"), 0o644)
	bad, _, _ := m.Create("badcontent", nil)
	bad.FS().WriteFile("/b", []byte("b"), 0o644)
	m.Create("badrecord", nil)
	m.Flush(context.Background())
	// Corrupt one volume's content and another's record; plant an orphan.
	bad.FS() // keep vet quiet about bad
	for _, k := range mustKeys(t, store, "vblob/") {
		if k != good.blobKey {
			store.Put(k, []byte("not a layer"))
		}
	}
	rk := recordKey("badrecord")
	v, _ := store.Get(rk)
	v[len(v)-2] ^= 1
	store.Put(rk, v)
	store.Put("vblob/"+strings.Repeat("e", 64), []byte("orphan"))

	m2 := New(Options{Store: store})
	rep, err := m2.Restore(lim)
	if err != nil || len(rep) != 2 {
		t.Fatalf("%v %v", rep, err)
	}
	if _, err := m2.Get("badrecord"); err == nil {
		t.Fatal("a corrupt record was kept")
	}
	if v, err := m2.Get("badcontent"); err != nil || v.Size() != 0 {
		t.Fatal("corrupt content kept", err)
	}
	if v, _ := m2.Get("good"); v == nil {
		t.Fatal("good lost")
	} else if b, _ := v.FS().ReadFile("/a"); string(b) != "a" {
		t.Fatal("good lost its content")
	}
	if _, err := store.Get("vblob/" + strings.Repeat("e", 64)); err == nil {
		t.Fatal("orphan kept")
	}
}

func mustKeys(t *testing.T, s state.Store, prefix string) []string {
	ks, err := s.Keys(prefix)
	if err != nil {
		t.Fatal(err)
	}
	return ks
}

func FuzzDecodeRecord(f *testing.F) {
	f.Add(state.Seal(recordKind, []byte(`{"name":"ab","blob":"vblob/`+strings.Repeat("0", 64)+`"}`)))
	f.Fuzz(func(t *testing.T, b []byte) {
		r, err := DecodeRecord(b)
		if err == nil && !ValidName(r.Name) {
			t.Fatal("accepted an invalid name")
		}
	})
}
