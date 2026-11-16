package wazero

import (
	"context"
	"testing"

	"github.com/go-wasmcontainers/engine/audio"
	"github.com/go-wasmcontainers/engine/audio/audiotest"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
)

// TestAudioModule runs testdata/wasm/audioprobe, a real guest, against
// wctr_audio_out: the ABI end to end (values in, results out of the guest's
// own linear memory), the grant, and the hostile cases.
func TestAudioModule(t *testing.T) {
	img := lower(t, "audioprobe")
	run1 := func(caps []string, prov audio.Provider, arg string) (run, *audiotest.Provider) {
		t.Helper()
		var ap *audiotest.Provider
		if p, ok := prov.(*audiotest.Provider); ok {
			ap = p
		}
		o := opts()
		o.HostModules = []wasi.HostModule{audio.New(prov)}
		e := New(o)
		defer e.Close(context.Background())
		r := start(t, e, runtime.Spec{ID: "snd", FS: img.Clone(rootfs.Quota{}), Path: "/bin/audioprobe.wasm",
			Args: []string{"audioprobe", arg}, Caps: caps})
		if r.res.ExitCode != 0 {
			t.Fatalf("%s: %+v (stderr %q)", arg, r.res, r.stderr)
		}
		return r, ap
	}

	// Granted, with a provider: a tone reaches the ring. Nobody plays yet (no
	// heartbeat), so the write is accepted and discarded: waiting.
	prov := &audiotest.Provider{}
	r, _ := run1([]string{"audio"}, prov, "tone")
	if want := "write 0 512 0\nstats 0 0\nclose 0\n"; r.stdout != want {
		t.Errorf("tone: %q, want %q", r.stdout, want)
	}
	if len(prov.Rings) != 1 || prov.Rings[0].Rate != 11025 || prov.Rings[0].Channels != 2 || prov.Rings[0].Capacity != 1024 {
		t.Fatalf("rings %+v", prov.Rings)
	}
	if !prov.Rings[0].Writer().Closed() || prov.Rings[0].CloseCalls() != 1 {
		t.Error("the stream is still open after the guest closed it")
	}

	// Not granted: Denied (2). No provider: Unsupported (3).
	if r, p := run1(nil, &audiotest.Provider{}, "tone"); r.stdout != "open 2\n" || len(p.Rings) != 0 {
		t.Errorf("denied: %q, %d rings", r.stdout, len(p.Rings))
	}
	if r, _ := run1([]string{"audio"}, nil, "tone"); r.stdout != "open 3\n" {
		t.Errorf("unsupported: %q", r.stdout)
	}

	// Hostile: a pointer at the 32-bit edge is a Fault (9); a second stream is
	// fine and the third is QuotaExceeded (5). Process end closes both.
	prov = &audiotest.Provider{}
	r, _ = run1([]string{"audio"}, prov, "hostile")
	if want := "edge 9\nsecond 0\nthird 5\nclose 0\n"; r.stdout != want {
		t.Errorf("hostile: %q, want %q", r.stdout, want)
	}
	for k, ring := range prov.Rings {
		if !ring.Writer().Closed() || ring.CloseCalls() != 1 {
			t.Errorf("ring %d left open after the process ended", k)
		}
	}
}
