package state

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stores(t *testing.T) map[string]Store {
	d, err := OpenDir(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	return map[string]Store{"mem": &Mem{}, "dir": d}
}

func TestStores(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if _, err := s.Get("container/abc"); !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
			if err := s.Put("container/abc", []byte("one")); err != nil {
				t.Fatal(err)
			}
			if err := s.Put("container/abc", []byte("two")); err != nil {
				t.Fatal(err)
			}
			s.Put("blob/00ff", []byte("b"))
			if v, err := s.Get("container/abc"); err != nil || string(v) != "two" {
				t.Fatal(string(v), err)
			}
			if ks, _ := s.Keys("container/"); strings.Join(ks, ",") != "container/abc" {
				t.Fatal(ks)
			}
			if ks, _ := s.Keys(""); len(ks) != 2 {
				t.Fatal(ks)
			}
			if err := s.Delete("container/abc"); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete("container/abc"); err != nil {
				t.Fatal("deleting an absent key:", err)
			}
			for _, bad := range []string{"", "x", "/x/y", "a/../b", "A/b", "a/b/c", "a/.b", "a/" + strings.Repeat("f", 200)} {
				if err := s.Put(bad, nil); !errors.Is(err, ErrBadKey) {
					t.Errorf("%q: %v", bad, err)
				}
			}
		})
	}
}

func TestDirLeavesNoTempFiles(t *testing.T) {
	root := t.TempDir()
	d, _ := OpenDir(root)
	// A crash between write and rename leaves a temporary file: it is
	// neither a key nor kept.
	os.MkdirAll(filepath.Join(root, "blob"), 0o700)
	os.WriteFile(filepath.Join(root, "blob", ".tmp-123"), []byte("torn"), 0o600)
	d.Put("blob/ab", []byte("x"))
	ks, err := d.Keys("")
	if err != nil || strings.Join(ks, ",") != "blob/ab" {
		t.Fatal(ks, err)
	}
	if _, err := os.Stat(filepath.Join(root, "blob", ".tmp-123")); err == nil {
		t.Fatal("temporary file kept")
	}
}

func TestMemQuota(t *testing.T) {
	s := &Mem{MaxBytes: 10}
	if err := s.Put("blob/a", make([]byte, 8)); err != nil {
		t.Fatal(err)
	}
	if err := s.Put("blob/b", make([]byte, 3)); !errors.Is(err, ErrNoSpace) {
		t.Fatal(err)
	}
	if _, err := s.Get("blob/b"); !errors.Is(err, ErrNotFound) {
		t.Fatal("a failed Put wrote something")
	}
	s.Put("blob/a", make([]byte, 2)) // shrinking frees room
	if err := s.Put("blob/b", make([]byte, 8)); err != nil || s.Size() != 10 {
		t.Fatal(err, s.Size())
	}
}

func TestSeal(t *testing.T) {
	v := Seal("container", []byte(`{"id":"x"}`))
	p, err := Unseal("container", v, 1<<20)
	if err != nil || string(p) != `{"id":"x"}` {
		t.Fatal(string(p), err)
	}
	for name, bad := range map[string][]byte{
		"other kind": Seal("logs", []byte("x")),
		"torn":       v[:len(v)-1],
		"flipped":    append(append([]byte{}, v[:len(v)-2]...), 'X', '}'),
		"extra":      append(append([]byte{}, v...), ' '),
		"no header":  []byte(`{"id":"x"}`),
		"empty":      nil,
		"too big":    Seal("container", bytes.Repeat([]byte("x"), 100)),
		"plus len":   bytes.Replace(v, []byte(" 10\n"), []byte(" +10\n"), 1),
		"zero len":   bytes.Replace(v, []byte(" 10\n"), []byte(" 010\n"), 1),
	} {
		if _, err := Unseal("container", bad, 50); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestBlob(t *testing.T) {
	s := &Mem{}
	k := BlobKey([]byte("data"))
	s.Put(k, []byte("data"))
	if b, err := GetBlob(s, k, 10); err != nil || string(b) != "data" {
		t.Fatal(err)
	}
	s.Put(k, []byte("dato"))
	if _, err := GetBlob(s, k, 10); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
	if _, err := GetBlob(s, k, 2); !errors.Is(err, ErrCorrupt) {
		t.Fatal(err)
	}
}

// FuzzUnseal: whatever storage hands back, Unseal accepts only what Seal
// made, and never panics.
func FuzzUnseal(f *testing.F) {
	f.Add(Seal("container", []byte(`{"id":"x"}`)))
	f.Add([]byte("wctr-state 1 container 00 1\nx"))
	f.Fuzz(func(t *testing.T, v []byte) {
		p, err := Unseal("container", v, 1<<16)
		if err != nil {
			return
		}
		if !bytes.Equal(Seal("container", p), v) {
			t.Fatalf("accepted a value Seal would not write: %q", v)
		}
	})
}
