package rootfs

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"

	"github.com/go-wasmcontainers/engine/internal/testutil"
)

func zstdOf(t testing.TB, b []byte, opts ...zstd.EOption) []byte {
	t.Helper()
	var out bytes.Buffer
	w, err := zstd.NewWriter(&out, opts...)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(b)
	w.Close()
	return out.Bytes()
}

func TestZstdLayer(t *testing.T) {
	layer := zstdOf(t, testutil.Tar(E{Name: "bin/lua.wasm", Body: []byte("\x00asm")}, E{Name: "etc/x", Body: []byte("x")}))
	for _, mt := range []string{mtTarZstd, mtPkgxZstd} {
		f := New()
		if err := f.ApplyLayer(bytes.NewReader(layer), mt, "", NewBudget(DefaultLimits)); err != nil {
			t.Fatalf("%s: %v", mt, err)
		}
		if b, _ := f.ReadFile("/bin/lua.wasm"); string(b) != "\x00asm" {
			t.Fatalf("%s: %q", mt, b)
		}
	}
	// The same ratio guard as gzip.
	bomb := zstdOf(t, testutil.Tar(E{Name: "zeros", Body: make([]byte, 64<<20)}))
	if err := New().ApplyLayer(bytes.NewReader(bomb), mtTarZstd, "", NewBudget(DefaultLimits)); !errors.Is(err, ErrLayer) || !strings.Contains(err.Error(), "ratio") {
		t.Fatalf("bomb: %v", err)
	}
	// A frame declaring a window over the bound is refused before it is
	// allocated.
	huge := zstdOf(t, testutil.Tar(E{Name: "a", Body: bytes.Repeat([]byte("x"), 1<<20)}), zstd.WithWindowSize(256<<20))
	if err := New().ApplyLayer(bytes.NewReader(huge), mtTarZstd, "", NewBudget(DefaultLimits)); !errors.Is(err, ErrLayer) {
		t.Fatalf("huge window: %v", err)
	}
	if err := New().ApplyLayer(strings.NewReader("not zstd"), mtPkgxZstd, "", NewBudget(DefaultLimits)); !errors.Is(err, ErrLayer) {
		t.Fatalf("garbage: %v", err)
	}
}
