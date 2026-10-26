package rootfs

import (
	"bytes"
	"testing"

	"github.com/go-wasmcontainers/engine/internal/testutil"
)

// BenchmarkUnpackGzipLayer measures applying a tar+gzip layer holding a
// real wasip1 module (about 1.5 MiB) to an empty tree.
func BenchmarkUnpackGzipLayer(b *testing.B) {
	mod := testutil.Wasm(b, "hello")
	layer := testutil.Gzip(testutil.Tar(E{Name: "bin/hello.wasm", Body: mod, Mode: 0o755}, E{Name: "etc/motd", Body: []byte("hi\n")}))
	b.SetBytes(int64(len(mod)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := New().ApplyLayer(bytes.NewReader(layer), mtTarGz, "", NewBudget(DefaultLimits)); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkClone measures what a container create pays for its root: a
// copy-on-write clone, then one write that copies up a directory chain.
func BenchmarkClone(b *testing.B) {
	lower := New()
	for i := 0; i < 200; i++ {
		lower.MkdirAll("/usr/lib/d"+string(rune('a'+i%26)), 0o755)
	}
	lower.WriteFile("/usr/lib/da/f", make([]byte, 1<<20), 0o644)
	lower.Freeze()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := lower.Clone(Quota{})
		if err := c.WriteFile("/usr/lib/da/new", []byte("x"), 0o644); err != nil {
			b.Fatal(err)
		}
	}
}
