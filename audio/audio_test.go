package audio_test

import (
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/audio"
	"github.com/go-wasmcontainers/engine/audio/audiotest"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/wasi"
)

// mem is guest memory with bounds computed in 64 bits.
type mem []byte

func (m mem) Read(off, n uint32) ([]byte, bool) {
	if uint64(off)+uint64(n) > uint64(len(m)) {
		return nil, false
	}
	return m[off : off+n], true
}

func (m mem) Write(off uint32, b []byte) bool {
	if uint64(off)+uint64(len(b)) > uint64(len(m)) {
		return false
	}
	copy(m[off:], b)
	return true
}

// wrapMem adds in 32 bits, as a careless executor would: the module must not
// depend on its Memory to catch an address that wraps.
type wrapMem struct{ mem }

func (m wrapMem) Read(off, n uint32) ([]byte, bool) {
	if off+n > uint32(len(m.mem)) { // wraps
		return nil, false
	}
	if n > uint32(len(m.mem)) || off > uint32(len(m.mem)) {
		// A wrapped sum that looks in range: hand back zeros, as an executor
		// that then slices [off:off+n] would (or panic). Either is the bug.
		return make([]byte, n), true
	}
	return m.mem[off : off+n], true
}

const (
	fnOpen  = 0
	fnWrite = 1
	fnStats = 2
	fnClose = 3
)

type harness struct {
	t      *testing.T
	p      *wasi.Proc
	m      mem
	mod    *audio.Module
	prov   *audiotest.Provider
	events *[]string
	clock  *time.Time
}

type harnessOpts struct {
	caps  []string
	prov  *audiotest.Provider // nil: use the module's own
	noPrv bool                // a nil provider
	mod   *audio.Module
	label string
}

func newHarness(t *testing.T, o harnessOpts) *harness {
	t.Helper()
	h := &harness{t: t, m: make(mem, 1<<16), events: new([]string), clock: new(time.Time)}
	*h.clock = time.Unix(1000, 0)
	switch {
	case o.mod != nil:
		h.mod = o.mod
	case o.noPrv:
		h.mod = audio.New(nil)
	default:
		h.prov = o.prov
		if h.prov == nil {
			h.prov = &audiotest.Provider{}
		}
		h.mod = audio.New(h.prov)
	}
	h.mod.Now = func() time.Time { return *h.clock }
	var mu sync.Mutex
	evs := h.events
	h.p = wasi.New(wasi.Config{
		FS: rootfs.New(), Modules: []wasi.HostModule{h.mod}, Caps: o.caps, Label: o.label,
		OnEvent: func(module, action string, attrs map[string]string) {
			mu.Lock()
			defer mu.Unlock()
			*evs = append(*evs, module+" "+action+" "+attrs["container"])
		},
	})
	t.Cleanup(h.p.Close)
	return h
}

var audioCap = []string{"audio"}

func (h *harness) call(fn int, args ...uint32) uint32 {
	h.t.Helper()
	a := make([]uint64, len(args))
	for i, v := range args {
		a[i] = uint64(v)
	}
	r := h.p.CallImport(len(wasi.Funcs)+fn, a, h.m)
	if r.Trap != "" {
		h.t.Fatalf("fn %d trapped: %s", fn, r.Trap)
	}
	return uint32(r.Ret)
}

func (h *harness) u32(off int) uint32 { return binary.LittleEndian.Uint32(h.m[off:]) }

// open opens a stream with the output slot at 0 and returns its handle.
func (h *harness) open(rate, ch, capacity uint32) uint32 {
	h.t.Helper()
	if e := h.call(fnOpen, rate, ch, capacity, 0); e != audio.Success {
		h.t.Fatalf("stream_open(%d,%d,%d) = %d", rate, ch, capacity, e)
	}
	return h.u32(0)
}

// put stores frames of a constant value at ptr.
func (h *harness) put(ptr uint32, frames, ch int, v float32) {
	for k := 0; k < frames*ch; k++ {
		binary.LittleEndian.PutUint32(h.m[int(ptr)+4*k:], math.Float32bits(v))
	}
}

// write writes frames of v and returns accepted and queued.
func (h *harness) write(s uint32, frames, ch int, v float32) (accepted, queued uint32) {
	h.t.Helper()
	h.put(4096, frames, ch, v)
	if e := h.call(fnWrite, s, 4096, uint32(frames), 8); e != audio.Success {
		h.t.Fatalf("stream_write = %d", e)
	}
	return h.u32(8), h.u32(12)
}

func (h *harness) stats(s uint32) (played, underruns, refused, state uint32) {
	h.t.Helper()
	if e := h.call(fnStats, s, 16); e != audio.Success {
		h.t.Fatalf("stream_stats = %d", e)
	}
	return h.u32(16), h.u32(20), h.u32(24), h.u32(28)
}

func TestFuncs(t *testing.T) {
	m := audio.New(nil)
	if m.Module() != "wctr_audio_out" {
		t.Fatalf("module name %q", m.Module())
	}
	imps, err := wasi.Imports([]wasi.HostModule{m})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range imps[len(wasi.Funcs):] {
		got = append(got, i.Name+"/"+i.Params+"/"+i.Result)
	}
	want := []string{"stream_open/iiii/i", "stream_write/iiii/i", "stream_stats/ii/i", "stream_close/i/i"}
	if len(got) != len(want) {
		t.Fatalf("imports %v", got)
	}
	for k := range want {
		if got[k] != want[k] {
			t.Errorf("import %d = %s, want %s", k, got[k], want[k])
		}
	}
}

func TestOpenRefusals(t *testing.T) {
	for _, c := range []struct {
		name               string
		rate, ch, capacity uint32
		want               uint32
	}{
		{"rate 7999", 7999, 2, 1024, audio.InvalidArgument},
		{"rate 48001", 48001, 2, 1024, audio.InvalidArgument},
		{"rate 0", 0, 2, 1024, audio.InvalidArgument},
		{"rate max u32", 0xFFFFFFFF, 2, 1024, audio.InvalidArgument},
		{"channels 0", 11025, 0, 1024, audio.InvalidArgument},
		{"channels 3", 11025, 3, 1024, audio.InvalidArgument},
		{"capacity 255", 11025, 2, 255, audio.InvalidArgument},
		{"capacity 300", 11025, 2, 300, audio.InvalidArgument},
		{"capacity 32768", 11025, 2, 32768, audio.InvalidArgument},
		{"capacity 0", 11025, 2, 0, audio.InvalidArgument},
		{"rate 8000 mono 256", 8000, 1, 256, audio.Success},
		{"rate 48000 stereo 16384", 48000, 2, 16384, audio.Success},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarness(t, harnessOpts{caps: audioCap})
			if got := h.call(fnOpen, c.rate, c.ch, c.capacity, 0); got != c.want {
				t.Errorf("stream_open = %d, want %d", got, c.want)
			}
			if c.want != audio.Success && len(h.prov.Rings) != 0 {
				t.Error("a refusal opened a ring")
			}
		})
	}
}

func TestDeniedAndUnsupported(t *testing.T) {
	// No capability: every function answers Denied, and nothing is opened.
	h := newHarness(t, harnessOpts{caps: []string{"gpu"}})
	for fn, args := range [][]uint32{{11025, 2, 1024, 0}, {1, 0, 0, 8}, {1, 16}, {1}} {
		if got := h.call(fn, args...); got != audio.Denied {
			t.Errorf("fn %d without the capability = %d, want Denied", fn, got)
		}
	}
	if len(h.prov.Rings) != 0 {
		t.Error("a denied process opened a ring")
	}
	// The capability but no provider (native wctr): Unsupported.
	n := newHarness(t, harnessOpts{caps: audioCap, noPrv: true})
	for fn, args := range [][]uint32{{11025, 2, 1024, 0}, {1, 0, 0, 8}, {1, 16}, {1}} {
		if got := n.call(fn, args...); got != audio.Unsupported {
			t.Errorf("fn %d with no provider = %d, want Unsupported", fn, got)
		}
	}
}

func TestProviderErrors(t *testing.T) {
	prov := &audiotest.Provider{Err: audio.ErrQuota}
	h := newHarness(t, harnessOpts{caps: audioCap, prov: prov})
	if got := h.call(fnOpen, 11025, 2, 1024, 0); got != audio.QuotaExceeded {
		t.Errorf("provider quota = %d", got)
	}
	prov.Err = errors.New("no page")
	if got := h.call(fnOpen, 11025, 2, 1024, 0); got != audio.Unsupported {
		t.Errorf("provider failure = %d", got)
	}
	// A failed open gave its slot back.
	prov.Err = nil
	h.open(11025, 2, 1024)
	h.open(11025, 2, 1024)
}

func TestQuotas(t *testing.T) {
	mod := audio.New(&audiotest.Provider{})
	var hs []*harness
	for c := 0; c < 5; c++ {
		hs = append(hs, newHarness(t, harnessOpts{caps: audioCap, mod: mod, label: string(rune('a' + c))}))
	}
	// Two streams per container; the third is refused.
	hs[0].open(11025, 2, 1024)
	s2 := hs[0].open(11025, 2, 1024)
	if got := hs[0].call(fnOpen, 11025, 2, 1024, 0); got != audio.QuotaExceeded {
		t.Errorf("third stream of a container = %d, want QuotaExceeded", got)
	}
	// Closing one makes room again.
	if got := hs[0].call(fnClose, s2); got != audio.Success {
		t.Fatalf("close = %d", got)
	}
	hs[0].open(11025, 2, 1024)
	// Page: 8 streams over all containers (2+2+2+2), the ninth is refused.
	for c := 1; c < 4; c++ {
		hs[c].open(11025, 2, 1024)
		hs[c].open(11025, 2, 1024)
	}
	if got := hs[4].call(fnOpen, 11025, 2, 1024, 0); got != audio.QuotaExceeded {
		t.Errorf("ninth stream of the page = %d, want QuotaExceeded", got)
	}
	// A process of the same container shares the container's quota.
	hs[0].p.Close()
	hs[4].open(11025, 2, 1024) // the page has room again
}

func TestHandles(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	if got := h.call(fnWrite, 99, 4096, 1, 8); got != audio.InvalidArgument {
		t.Errorf("write on unknown handle = %d", got)
	}
	if got := h.call(fnStats, 99, 16); got != audio.InvalidArgument {
		t.Errorf("stats on unknown handle = %d", got)
	}
	if got := h.call(fnClose, 99); got != audio.InvalidArgument {
		t.Errorf("close of unknown handle = %d", got)
	}
	if got := h.call(fnClose, 0); got != audio.InvalidArgument {
		t.Errorf("close of handle 0 = %d", got)
	}
	s := h.open(11025, 2, 1024)
	if got := h.call(fnClose, s); got != audio.Success {
		t.Fatalf("close = %d", got)
	}
	if got := h.call(fnClose, s); got != audio.InvalidArgument {
		t.Errorf("second close = %d", got)
	}
	if got := h.call(fnWrite, s, 4096, 1, 8); got != audio.InvalidArgument {
		t.Errorf("write after close = %d", got)
	}
	if h.prov.Rings[0].CloseCalls() != 1 {
		t.Errorf("sink closed %d times", h.prov.Rings[0].CloseCalls())
	}
	// A handle is not reused: a stale one cannot reach a newer stream.
	if s2 := h.open(11025, 2, 1024); s2 == s {
		t.Errorf("handle %d reused", s)
	}
}

func TestFaults(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	size := uint32(len(h.m))
	// stream_open: the output slot outside memory, at the 32-bit edge, and
	// with an address that wraps. No stream is left behind.
	for _, out := range []uint32{size, size - 3, 0xFFFFFFFC, 0xFFFFFFFF} {
		if got := h.call(fnOpen, 11025, 2, 1024, out); got != audio.Fault {
			t.Errorf("stream_open out=%#x = %d, want Fault", out, got)
		}
	}
	if len(h.prov.Rings) != 0 {
		t.Error("a Fault left a ring behind")
	}
	s := h.open(11025, 2, 1024)
	// stream_write: ptr+frames*channels*4 outside memory, or wrapping 32 bits.
	for _, c := range []struct{ ptr, frames uint32 }{
		{size - 4, 1}, {size, 1}, {0xFFFFFFF0, 256}, {0xFFFFFFF0, 1024}, {0xFFFFFFFC, 1}, {0xFFFFFFFF, 1},
	} {
		if got := h.call(fnWrite, s, c.ptr, c.frames, 8); got != audio.Fault {
			t.Errorf("stream_write ptr=%#x frames=%d = %d, want Fault", c.ptr, c.frames, got)
		}
	}
	for _, out := range []uint32{size - 7, 0xFFFFFFF8, 0xFFFFFFFF} {
		if got := h.call(fnWrite, s, 0, 1, out); got != audio.Fault {
			t.Errorf("stream_write out=%#x = %d, want Fault", out, got)
		}
	}
	for _, out := range []uint32{size - 15, 0xFFFFFFF8, 0xFFFFFFFF} {
		if got := h.call(fnStats, s, out); got != audio.Fault {
			t.Errorf("stream_stats out=%#x = %d, want Fault", out, got)
		}
	}
	// A Fault queued nothing.
	h.call(fnStats, s, 16)
	if _, _, refused, _ := h.stats(s); refused != 0 || h.prov.Rings[0].Writer().Buffered() != 0 {
		t.Errorf("a Fault changed the stream: refused=%d", refused)
	}
}

// The module checks the range in 64 bits itself: an executor whose Memory
// adds in 32 bits (and so accepts 0xFFFFFFF0+2048 as 0x7F0) must not be able
// to read out of bounds through it.
func TestFaultDoesNotRelyOnMemory(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	if got := h.call(fnOpen, 11025, 2, 256, 0); got != 0 {
		t.Fatal(got)
	}
	s := h.u32(0)
	r := h.p.CallImport(len(wasi.Funcs)+fnWrite, []uint64{uint64(s), 0xFFFFFFF0, 256, 8}, wrapMem{h.m})
	if r.Trap != "" || uint32(r.Ret) != audio.Fault {
		t.Errorf("write at the 32-bit edge through a wrapping Memory = %+v, want Fault", r)
	}
	r = h.p.CallImport(len(wasi.Funcs)+fnOpen, []uint64{11025, 2, 256, 0xFFFFFFFE}, wrapMem{h.m})
	if r.Trap != "" || uint32(r.Ret) != audio.Fault {
		t.Errorf("open slot at the edge through a wrapping Memory = %+v, want Fault", r)
	}
}

func TestTooLarge(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	s := h.open(11025, 2, 1024)
	if got := h.call(fnWrite, s, 4096, 1025, 8); got != audio.TooLarge {
		t.Errorf("frames above capacity = %d, want TooLarge", got)
	}
	if got := h.call(fnWrite, s, 4096, 0xFFFFFFFF, 8); got != audio.TooLarge {
		t.Errorf("frames = 2^32-1 = %d, want TooLarge", got)
	}
	// The copy is bounded by the largest stream: 128 KiB.
	big := newHarness(t, harnessOpts{caps: audioCap})
	b := big.open(48000, 2, 16384)
	big.m = make(mem, 1<<18)
	if got := big.call(fnWrite, b, 0, 16384, 1<<17+64); got != audio.Success {
		t.Errorf("a full-capacity write = %d", got)
	}
	if audio.MaxCopy != 128<<10 {
		t.Errorf("MaxCopy = %d", audio.MaxCopy)
	}
}

// start plays the worklet: its heartbeat moves, and one call samples it.
func start(h *harness, r *audiotest.Ring, s uint32) {
	h.t.Helper()
	r.Beat()
	if _, _, _, st := h.stats(s); st != audio.StatePlaying {
		h.t.Fatalf("state after a heartbeat = %d", st)
	}
}

func TestWriteAgainstModelConsumer(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	s := h.open(11025, 2, 1024)
	r := h.prov.Last()
	start(h, r, s)

	if a, q := h.write(s, 300, 2, 0.5); a != 300 || q != 300 {
		t.Fatalf("accepted %d queued %d, want 300 300", a, q)
	}
	// frames = 0 is a poll.
	if a, q := h.write(s, 0, 2, 0); a != 0 || q != 300 {
		t.Fatalf("poll: accepted %d queued %d", a, q)
	}
	got := r.Consume(100)
	if len(got) != 200 || got[0] != 0.5 || got[199] != 0.5 {
		t.Fatalf("consumed %d samples, first %v", len(got), got[:1])
	}
	// 200 queued, 824 free: 900 offered, 824 accepted, 76 refused.
	if a, q := h.write(s, 900, 2, 0.25); a != 824 || q != 1024 {
		t.Fatalf("accepted %d queued %d, want 824 1024", a, q)
	}
	played, under, refused, state := h.stats(s)
	if played != 100 || under != 0 || refused != 76 || state != audio.StatePlaying {
		t.Errorf("stats = played %d underruns %d refused %d state %d", played, under, refused, state)
	}
	// The ring wraps correctly: 200 of 0.5 first, then 824 of 0.25.
	all := r.Consume(2000)
	if len(all) != 2*1024 || all[0] != 0.5 || all[2*200-1] != 0.5 || all[2*200] != 0.25 || all[len(all)-1] != 0.25 {
		t.Errorf("ring content wrong over the wrap (%d samples)", len(all))
	}
	if a, q := h.write(s, 10, 2, 1); a != 10 || q != 10 {
		t.Errorf("after draining: accepted %d queued %d", a, q)
	}
}

func TestMono(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	s := h.open(22050, 1, 256)
	r := h.prov.Last()
	start(h, r, s)
	if a, q := h.write(s, 256, 1, 0.1); a != 256 || q != 256 {
		t.Fatalf("mono: accepted %d queued %d", a, q)
	}
	if a, _ := h.write(s, 1, 1, 0.1); a != 0 {
		t.Errorf("a full ring accepted %d", a)
	}
	if r.Consume(10); len(r.Consume(1)) != 1 {
		t.Error("mono consume")
	}
}

func TestSanitize(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	s := h.open(11025, 1, 256)
	r := h.prov.Last()
	start(h, r, s)
	in := []float32{float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1)), 2, -3, 0.5, -0.5, 1, -1,
		math.Float32frombits(0x7FC00001), math.MaxFloat32, -math.MaxFloat32, math.SmallestNonzeroFloat32}
	for k, v := range in {
		binary.LittleEndian.PutUint32(h.m[4096+4*k:], math.Float32bits(v))
	}
	if e := h.call(fnWrite, s, 4096, uint32(len(in)), 8); e != 0 {
		t.Fatal(e)
	}
	want := []float32{0, 0, 0, 1, -1, 0.5, -0.5, 1, -1, 0, 1, -1, math.SmallestNonzeroFloat32}
	got := r.Consume(len(in))
	for k := range want {
		if got[k] != want[k] {
			t.Errorf("sample %d: %v -> %v, want %v", k, in[k], got[k], want[k])
		}
	}
	for k, v := range got {
		if v != v || v > 1 || v < -1 {
			t.Errorf("sample %d left the ring as %v", k, v)
		}
	}
}

func TestWaitingBeforeThePagePlays(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	s := h.open(11025, 2, 1024)
	r := h.prov.Last()

	// No heartbeat yet: accepted, discarded, never queued, not counted refused.
	if a, q := h.write(s, 500, 2, 0.5); a != 500 || q != 0 {
		t.Fatalf("waiting: accepted %d queued %d, want 500 0", a, q)
	}
	if r.Writer().Buffered() != 0 {
		t.Fatal("sound written before the page plays was queued")
	}
	if _, _, refused, state := h.stats(s); refused != 0 || state != audio.StateWaiting {
		t.Errorf("waiting stats: refused %d state %d", refused, state)
	}

	// The worklet runs: now it plays.
	r.Beat()
	if a, q := h.write(s, 100, 2, 0.5); a != 100 || q != 100 {
		t.Fatalf("playing: accepted %d queued %d", a, q)
	}
	// The heartbeat stands still for longer than 200 ms: waiting again, and the
	// audio already queued is not touched by the discarded writes.
	*h.clock = h.clock.Add(audio.HeartbeatStale + time.Millisecond)
	if a, q := h.write(s, 100, 2, 0.5); a != 100 || q != 0 {
		t.Fatalf("stale: accepted %d queued %d, want 100 0", a, q)
	}
	if r.Writer().Buffered() != 100 {
		t.Errorf("a stale write changed the ring: %d buffered", r.Writer().Buffered())
	}
	// Exactly at the bound it still plays (a heartbeat seen at t, now t+200ms).
	r.Beat()
	h.write(s, 0, 2, 0) // sample the new beat at the new time
	*h.clock = h.clock.Add(audio.HeartbeatStale)
	if _, _, _, st := h.stats(s); st != audio.StatePlaying {
		t.Errorf("a heartbeat exactly %v old: state %d, want playing", audio.HeartbeatStale, st)
	}
	*h.clock = h.clock.Add(time.Nanosecond)
	if _, _, _, st := h.stats(s); st != audio.StateWaiting {
		t.Errorf("a heartbeat older than the bound: state %d, want waiting", st)
	}
	r.Beat()
	if _, _, _, st := h.stats(s); st != audio.StatePlaying {
		t.Errorf("after a new heartbeat: state %d, want playing", st)
	}
}

func TestClosedByThePage(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	s := h.open(11025, 2, 1024)
	r := h.prov.Last()
	start(h, r, s)
	r.CloseFromPage()
	if a, q := h.write(s, 50, 2, 0.5); a != 0 || q != 0 {
		t.Errorf("closed ring: accepted %d queued %d", a, q)
	}
	if _, _, refused, state := h.stats(s); state != audio.StateClosed || refused != 50 {
		t.Errorf("closed stats: state %d refused %d", state, refused)
	}
}

func TestProcessEndClosesStreams(t *testing.T) {
	mod := audio.New(&audiotest.Provider{})
	h := newHarness(t, harnessOpts{caps: audioCap, mod: mod, label: "web"})
	a := h.open(11025, 2, 1024)
	_ = a
	b := h.open(11025, 2, 1024)
	_ = b
	rings := mod.Provider.(*audiotest.Provider).Rings
	h.p.Close()
	for k, r := range rings {
		if !r.Writer().Closed() || r.CloseCalls() != 1 {
			t.Errorf("ring %d: closed %v, close calls %d", k, r.Writer().Closed(), r.CloseCalls())
		}
	}
	// The slots are back: another process of the module can open two again.
	h2 := newHarness(t, harnessOpts{caps: audioCap, mod: mod, label: "web"})
	h2.open(11025, 2, 1024)
	h2.open(11025, 2, 1024)
	// And the events show open and close.
	want := []string{"wctr_audio_out open web", "wctr_audio_out open web", "wctr_audio_out close web", "wctr_audio_out close web"}
	if got := *h.events; len(got) != len(want) {
		t.Fatalf("events %v", got)
	} else {
		for k := range want {
			if got[k] != want[k] {
				t.Errorf("event %d = %q, want %q", k, got[k], want[k])
			}
		}
	}
}

// Closing the process while the guest is still calling is safe (go test
// -race): a call sees the closed instance and fails, never a half-torn one.
func TestCloseDuringCalls(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	s := h.open(11025, 2, 1024)
	r := h.prov.Last()
	r.Beat()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		m := make(mem, 1<<14)
		for k := 0; k < 2000; k++ {
			res := h.p.CallImport(len(wasi.Funcs)+fnWrite, []uint64{uint64(s), 4096, 64, 8}, m)
			if res.Trap == "" && res.Ret != audio.Success && res.Ret != audio.InvalidArgument {
				t.Errorf("write during close = %d", res.Ret)
				return
			}
		}
	}()
	time.Sleep(time.Millisecond)
	h.p.Close()
	wg.Wait()
	if r.CloseCalls() != 1 {
		t.Errorf("sink closed %d times", r.CloseCalls())
	}
}

func TestRefusedEventIsRateLimited(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	s := h.open(11025, 2, 256)
	start(h, h.prov.Last(), s)
	h.write(s, 256, 2, 0.1)
	for k := 0; k < 100; k++ {
		h.write(s, 16, 2, 0.1) // full: refused
	}
	count := func() (n int) {
		for _, e := range *h.events {
			if e == "wctr_audio_out refused " {
				n++
			}
		}
		return
	}
	if n := count(); n != 1 {
		t.Fatalf("%d refused events for 100 refusals within one second, want 1", n)
	}
	*h.clock = h.clock.Add(2 * time.Second)
	h.prov.Last().Beat() // the page still plays
	h.write(s, 16, 2, 0.1)
	if n := count(); n != 2 {
		t.Errorf("%d refused events after the interval, want 2", n)
	}
	if _, _, refused, _ := h.stats(s); refused != 101*16 {
		t.Errorf("refused = %d, want %d", refused, 101*16)
	}
}

// A sequence of calls decoded from data never panics, never queues more than
// the ring holds, and always leaves the quotas whole once the process ends.
func FuzzCall(f *testing.F) {
	f.Add([]byte{0, 0x11, 0x2B, 0, 0, 2, 0, 0, 0, 0, 4, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{1, 1, 0, 0, 0, 0x10, 0, 0, 0, 5, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		mod := audio.New(&audiotest.Provider{})
		p := wasi.New(wasi.Config{FS: rootfs.New(), Modules: []wasi.HostModule{mod}, Caps: audioCap, Label: "fz"})
		m := make(mem, 1<<15)
		for k := 0; k+17 <= len(data); k += 17 {
			fn := int(data[k] % 4)
			var a [4]uint64
			for j := range a {
				a[j] = uint64(binary.LittleEndian.Uint32(data[k+1+4*j:]))
			}
			args := a[:]
			switch fn {
			case fnStats:
				args = a[:2]
			case fnClose:
				args = a[:1]
			}
			// Steer toward live handles and small addresses half the time.
			if data[k]&0x80 != 0 {
				args[0] = args[0]%4 + 1
				if fn == fnWrite {
					args[1] %= 1 << 14
					args[3] %= 1 << 14
				}
			}
			r := p.CallImport(len(wasi.Funcs)+fn, args, m)
			if r.Trap != "" {
				t.Fatalf("fn %d args %v trapped: %s", fn, args, r.Trap)
			}
			switch r.Ret {
			case audio.Success, audio.InvalidArgument, audio.TooLarge, audio.QuotaExceeded, audio.Fault:
			default:
				t.Fatalf("fn %d args %v: unexpected errno %d", fn, args, r.Ret)
			}
		}
		for _, r := range mod.Provider.(*audiotest.Provider).Rings {
			if r.Writer().Buffered() > r.Capacity {
				t.Fatalf("ring holds %d frames, capacity %d", r.Writer().Buffered(), r.Capacity)
			}
		}
		p.Close()
		for _, r := range mod.Provider.(*audiotest.Provider).Rings {
			if r.CloseCalls() != 1 {
				t.Fatalf("a ring was closed %d times", r.CloseCalls())
			}
		}
		// Every slot is back.
		q := wasi.New(wasi.Config{FS: rootfs.New(), Modules: []wasi.HostModule{mod}, Caps: audioCap, Label: "fz"})
		defer q.Close()
		for k := 0; k < audio.StreamsPerContainer; k++ {
			if r := q.CallImport(len(wasi.Funcs)+fnOpen, []uint64{11025, 2, 256, 0}, m); uint32(r.Ret) != audio.Success {
				t.Fatalf("slot %d not released: %d", k, r.Ret)
			}
		}
	})
}
