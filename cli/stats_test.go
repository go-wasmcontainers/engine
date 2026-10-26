package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/api/server"
	"github.com/go-wasmcontainers/engine/container"
)

func TestStats(t *testing.T) {
	h := startDaemon(t, container.Options{MemoryLimitBytes: 64 << 20})
	layout := testLayout(t)
	must(t, wctr(t, h, "pull", "oci:"+layout+":probe"), 0)
	must(t, wctr(t, h, "run", "--name", "w", "probe", "fs"), 0)
	must(t, wctr(t, h, "run", "-d", "--name", "s", "-m", "32m", "probe", "spin"), 0)
	c, _ := api.NewClient(h)
	ctx := context.Background()

	// A running guest: its linear memory, under its own limit, one pid.
	var s api.ContainerStats
	for i := 0; i < 100; i++ {
		c.Stats(ctx, "s", false, func(x api.ContainerStats) error { s = x; return nil })
		if s.MemoryStats.Usage > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if s.Name != "/s" || s.PidsStats.Current != 1 || s.MemoryStats.Usage == 0 || s.MemoryStats.Limit != 32<<20 || s.MemoryStats.Usage > s.MemoryStats.Limit {
		t.Fatalf("%+v", s)
	}
	if !s.Wctr.Running || !strings.Contains(s.Wctr.CPU, "not measured") {
		t.Fatalf("%+v", s.Wctr)
	}
	// An exited one: no memory, what it wrote, the default limit.
	c.Stats(ctx, "w", false, func(x api.ContainerStats) error { s = x; return nil })
	if s.PidsStats.Current != 0 || s.MemoryStats.Usage != 0 || s.MemoryStats.Limit != 64<<20 || blkioWrite(s) < uint64(len("alpha+beta")) || s.Wctr.SizeRw == 0 {
		t.Fatalf("%+v", s)
	}
	// A stream: one reading per interval, until the client leaves.
	sctx, cancel := context.WithCancel(ctx)
	n := 0
	server.StatsInterval = 50 * time.Millisecond
	defer func() { server.StatsInterval = time.Second }()
	c.Stats(sctx, "s", true, func(x api.ContainerStats) error {
		n++
		if n == 3 {
			if x.Preread == "0001-01-01T00:00:00Z" {
				t.Error("preread not set on a later reading")
			}
			cancel()
		}
		return nil
	})
	if n < 3 {
		t.Fatalf("%d readings", n)
	}
	r := must(t, wctr(t, h, "stats", "--no-stream", "-a"), 0)
	for _, want := range []string{"CONTAINER ID", "MEM USAGE / LIMIT", "s ", "w ", "/ 32.00MiB", "--"} {
		if !strings.Contains(r.stdout, want) {
			t.Fatalf("lacks %q:\n%s", want, r.stdout)
		}
	}
	must(t, wctr(t, h, "kill", "s"), 0)
}
