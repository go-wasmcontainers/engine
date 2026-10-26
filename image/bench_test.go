package image_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/internal/testutil"
)

// BenchmarkPullLayout measures a cold pull (verify + store) of a real
// wasip1 hello image, tar+gzip layer, from an OCI layout on disk.
func BenchmarkPullLayout(b *testing.B) {
	mod := testutil.Wasm(b, "hello")
	c := testutil.BuildContent(testutil.ImageSpec{RefName: "hello", Module: mod})
	dir := filepath.Join(b.TempDir(), "layout")
	testutil.WriteLayout(b, dir, c)
	var size int64
	for _, blob := range c.Blobs {
		size += int64(len(blob))
	}
	b.SetBytes(size)
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		st, err := image.OpenStore(filepath.Join(b.TempDir(), "s"))
		if err != nil {
			b.Fatal(err)
		}
		src, _, err := image.OpenSource("oci:"+dir+":hello", image.RegistryOptions{}, image.DefaultLimits)
		if err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := image.Pull(ctx, st, src, image.DefaultLimits, nil); err != nil {
			b.Fatal(err)
		}
	}
}
