package wazero

import (
	"context"
	"testing"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
)

// BenchmarkStartCold is a first container of an image: compile, then run
// hello to its exit. A new executor per iteration, so nothing is cached.
func BenchmarkStartCold(b *testing.B) {
	img := lower(b, "hello")
	ctx := context.Background()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e := New(Options{})
		p, err := e.Start(ctx, runtime.Spec{FS: img.Clone(rootfs.Quota{}), Path: "/bin/hello.wasm"})
		if err != nil {
			b.Fatal(err)
		}
		if r := p.Wait(); r.ExitCode != 0 {
			b.Fatal(r)
		}
		b.StopTimer()
		e.Close(ctx)
		b.StartTimer()
	}
}

// BenchmarkStartWarm is every later container of the same image: the
// compiled module comes from the per-digest cache.
func BenchmarkStartWarm(b *testing.B) {
	img := lower(b, "hello")
	ctx := context.Background()
	e := New(Options{})
	defer e.Close(ctx)
	warm, _ := e.Start(ctx, runtime.Spec{FS: img.Clone(rootfs.Quota{}), Path: "/bin/hello.wasm"})
	warm.Wait()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		p, err := e.Start(ctx, runtime.Spec{FS: img.Clone(rootfs.Quota{}), Path: "/bin/hello.wasm"})
		if err != nil {
			b.Fatal(err)
		}
		if r := p.Wait(); r.ExitCode != 0 {
			b.Fatal(r)
		}
	}
}
