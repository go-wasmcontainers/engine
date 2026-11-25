package audio_test

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/go-wasmcontainers/engine/audio"
	"github.com/go-wasmcontainers/engine/audio/audiotest"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/wasi"
)

// Function indexes of the voice calls, after the four stream calls.
const (
	fnVoiceOpen = 4 + iota
	fnSampleLoad
	fnVoiceCommit
	fnVoiceStats
	fnVoiceClose
)

func (h *harness) voiceOpen(rate uint32) uint32 {
	h.t.Helper()
	if e := h.call(fnVoiceOpen, rate, 0); e != audio.Success {
		h.t.Fatalf("voice_open(%d) = %d", rate, e)
	}
	return h.u32(0)
}

func TestVoiceFuncs(t *testing.T) {
	imps, err := wasi.Imports([]wasi.HostModule{audio.New(nil)})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, i := range imps[len(wasi.Funcs):] {
		got = append(got, i.Name+"/"+i.Params+"/"+i.Result)
	}
	want := []string{
		"stream_open/iiii/i", "stream_write/iiii/i", "stream_stats/ii/i", "stream_close/i/i",
		"voice_open/ii/i", "sample_load/iiiiiiii/i", "voice_commit/iiii/i", "voice_stats/ii/i", "voice_close/i/i",
	}
	if len(got) != len(want) {
		t.Fatalf("imports %v", got)
	}
	for k := range want {
		if got[k] != want[k] {
			t.Errorf("import %d = %s, want %s", k, got[k], want[k])
		}
	}
}

func TestVoiceOpenRefusals(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	for _, c := range []struct {
		name string
		rate uint32
		out  uint32
		want uint32
	}{
		{"rate below the minimum", audio.MinRate - 1, 0, audio.InvalidArgument},
		{"rate above the maximum", audio.MaxRate + 1, 0, audio.InvalidArgument},
		{"zero rate", 0, 0, audio.InvalidArgument},
		{"out slot outside memory", 11025, uint32(len(h.m)) - 3, audio.Fault},
		{"out slot wraps 32 bits", 11025, 0xFFFFFFFE, audio.Fault},
	} {
		if got := h.call(fnVoiceOpen, c.rate, c.out); got != c.want {
			t.Errorf("%s: errno %d, want %d", c.name, got, c.want)
		}
	}
	if n := len(h.prov.VoiceSinks()); n != 0 {
		t.Errorf("%d voice sinks opened by refused calls", n)
	}
	// A refused call leaves the quotas whole: the container still gets its streams.
	for k := 0; k < audio.StreamsPerContainer; k++ {
		h.open(11025, 2, 256)
	}
}

func TestVoiceDeniedAndUnsupported(t *testing.T) {
	if e := newHarness(t, harnessOpts{}).call(fnVoiceOpen, 11025, 0); e != audio.Denied {
		t.Errorf("not granted: %d, want %d", e, audio.Denied)
	}
	if e := newHarness(t, harnessOpts{caps: audioCap, noPrv: true}).call(fnVoiceOpen, 11025, 0); e != audio.Unsupported {
		t.Errorf("no provider: %d, want %d", e, audio.Unsupported)
	}
	// A provider that has rings but cannot host a mixer.
	mod := audio.New(plainProvider{inner: &audiotest.Provider{}})
	h := newHarness(t, harnessOpts{caps: audioCap, mod: mod})
	if e := h.call(fnVoiceOpen, 11025, 0); e != audio.Unsupported {
		t.Errorf("provider without voices: %d, want %d", e, audio.Unsupported)
	}
	// Not granted: the other voice calls are Denied too.
	d := newHarness(t, harnessOpts{})
	for _, c := range []struct {
		fn   int
		args []uint32
	}{{fnSampleLoad, make([]uint32, 8)}, {fnVoiceCommit, make([]uint32, 4)}, {fnVoiceStats, make([]uint32, 2)}, {fnVoiceClose, []uint32{1}}} {
		if e := d.call(c.fn, c.args...); e != audio.Denied {
			t.Errorf("fn %d not granted: %d, want %d", c.fn, e, audio.Denied)
		}
	}
}

func TestVoiceProviderErrors(t *testing.T) {
	prov := &audiotest.Provider{VoiceErr: errors.New("no mixer")}
	h := newHarness(t, harnessOpts{caps: audioCap, prov: prov})
	if e := h.call(fnVoiceOpen, 11025, 0); e != audio.Unsupported {
		t.Errorf("provider error: %d, want %d", e, audio.Unsupported)
	}
	prov.VoiceErr = audio.ErrQuota
	if e := h.call(fnVoiceOpen, 11025, 0); e != audio.QuotaExceeded {
		t.Errorf("provider quota: %d, want %d", e, audio.QuotaExceeded)
	}
	// Neither failure kept a stream slot.
	for k := 0; k < audio.StreamsPerContainer; k++ {
		h.open(11025, 2, 256)
	}
}

// A mixer takes a stream slot (it is a stream of the page), and a container has
// one mixer: the second voice_open is QuotaExceeded and keeps nothing.
func TestVoiceQuotas(t *testing.T) {
	prov := &audiotest.Provider{}
	mod := audio.New(prov)
	h := newHarness(t, harnessOpts{caps: audioCap, mod: mod, label: "a"})
	v := h.voiceOpen(11025)
	if e := h.call(fnVoiceOpen, 11025, 0); e != audio.QuotaExceeded {
		t.Errorf("second mixer of one container: %d, want %d", e, audio.QuotaExceeded)
	}
	s1 := h.open(11025, 2, 256) // the one stream slot left of the container
	if e := h.call(fnOpen, 11025, 2, 256, 0); e != audio.QuotaExceeded {
		t.Errorf("the mixer did not take a stream slot: %d, want %d", e, audio.QuotaExceeded)
	}
	if e := h.call(fnVoiceClose, v); e != audio.Success {
		t.Fatalf("voice_close = %d", e)
	}
	h.open(11025, 2, 256) // its slot is back
	if sinks := prov.VoiceSinks(); len(sinks) != 1 || sinks[0].CloseCalls() != 1 {
		t.Fatalf("sinks %d, closes %v", len(sinks), sinks)
	}
	// After the close the container may open a new mixer, if a slot is free.
	if e := h.call(fnClose, s1); e != audio.Success {
		t.Fatalf("stream_close = %d", e)
	}
	h.voiceOpen(11025)

	// Page wide: VoicesPerPage mixers across containers, the next is refused.
	prov2 := &audiotest.Provider{}
	mod2 := audio.New(prov2)
	var hs []*harness
	for k := 0; k < audio.VoicesPerPage; k++ {
		x := newHarness(t, harnessOpts{caps: audioCap, mod: mod2, label: string(rune('a' + k))})
		x.voiceOpen(11025)
		hs = append(hs, x)
	}
	extra := newHarness(t, harnessOpts{caps: audioCap, mod: mod2, label: "z"})
	if e := extra.call(fnVoiceOpen, 11025, 0); e != audio.QuotaExceeded {
		t.Errorf("mixer %d of the page: %d, want %d", audio.VoicesPerPage+1, e, audio.QuotaExceeded)
	}
	hs[0].p.Close()
	extra.voiceOpen(11025)
}

// A mixer refused for want of a stream slot must not keep a mixer slot of the
// page: VoicesPerPage containers all full of streams fail, then all succeed.
func TestVoiceRefusedForStreamsKeepsNoPageSlot(t *testing.T) {
	mod := audio.New(&audiotest.Provider{})
	var full []*harness
	for k := 0; k < audio.VoicesPerPage+1; k++ {
		x := newHarness(t, harnessOpts{caps: audioCap, mod: mod, label: string(rune('a' + k))})
		for s := 0; s < audio.StreamsPerContainer; s++ {
			if k < audio.StreamsPerPage/audio.StreamsPerContainer {
				x.open(11025, 2, 256)
			}
		}
		full = append(full, x)
	}
	for k := 0; k < audio.StreamsPerPage/audio.StreamsPerContainer; k++ {
		if e := full[k].call(fnVoiceOpen, 11025, 0); e != audio.QuotaExceeded {
			t.Fatalf("container %d, streams full: %d, want %d", k, e, audio.QuotaExceeded)
		}
	}
	// Nothing leaked: the page still has all its mixer slots once streams are free.
	for k := 0; k < audio.StreamsPerPage/audio.StreamsPerContainer; k++ {
		full[k].p.Close()
	}
	for k := 0; k < audio.VoicesPerPage; k++ {
		x := newHarness(t, harnessOpts{caps: audioCap, mod: mod, label: string(rune('m' + k))})
		x.voiceOpen(11025)
	}
}

// A memory that does not catch a wrapped address must not lead to a handle written
// out of bounds, nor to a mixer left open.
func TestVoiceOpenFaultKeepsNothing(t *testing.T) {
	prov := &audiotest.Provider{}
	h := newHarness(t, harnessOpts{caps: audioCap, prov: prov})
	if e := h.call(fnVoiceOpen, 11025, 0xFFFFFFFC); e != audio.Fault {
		t.Fatalf("out of range slot: %d, want %d", e, audio.Fault)
	}
	if n := len(prov.VoiceSinks()); n != 0 {
		t.Errorf("%d mixers opened for a faulting call", n)
	}
	h.voiceOpen(11025)
}

func TestVoiceHandlesAndWorkerServedCalls(t *testing.T) {
	h := newHarness(t, harnessOpts{caps: audioCap})
	v := h.voiceOpen(11025)
	if v == 0 {
		t.Fatal("handle 0")
	}
	s := h.open(11025, 2, 256)
	if s == v {
		t.Fatalf("stream and mixer share handle %d", v)
	}
	// An unknown handle is InvalidArgument for every call; a stream's handle is
	// not a mixer's.
	for _, bad := range []uint32{0, v + 100, s} {
		for _, c := range []struct {
			fn   int
			args []uint32
		}{{fnSampleLoad, []uint32{bad, 0, 0, 0, 8, 11025, 0, 0}}, {fnVoiceCommit, []uint32{bad, 0, 0, 0}}, {fnVoiceStats, []uint32{bad, 0}}, {fnVoiceClose, []uint32{bad}}} {
			if e := h.call(c.fn, c.args...); e != audio.InvalidArgument {
				t.Errorf("handle %d, fn %d: %d, want %d", bad, c.fn, e, audio.InvalidArgument)
			}
		}
	}
	// A mixer's handle is not a stream's.
	if e := h.call(fnWrite, v, 4096, 8, 8); e != audio.InvalidArgument {
		t.Errorf("stream_write on a mixer: %d, want %d", e, audio.InvalidArgument)
	}
	if e := h.call(fnClose, v); e != audio.InvalidArgument {
		t.Errorf("stream_close on a mixer: %d, want %d", e, audio.InvalidArgument)
	}
	// The browser's worker serves load, commit and stats; a call that reaches the
	// engine for a live mixer says so instead of pretending.
	for _, c := range []struct {
		fn   int
		args []uint32
	}{{fnSampleLoad, []uint32{v, 0, 0, 0, 8, 11025, 0, 0}}, {fnVoiceCommit, []uint32{v, 0, 0, 0}}, {fnVoiceStats, []uint32{v, 0}}} {
		if e := h.call(c.fn, c.args...); e != audio.Unsupported {
			t.Errorf("fn %d on a live mixer: %d, want %d (served by the guest's worker)", c.fn, e, audio.Unsupported)
		}
	}
	if e := h.call(fnVoiceClose, v); e != audio.Success {
		t.Fatalf("voice_close = %d", e)
	}
	if e := h.call(fnVoiceClose, v); e != audio.InvalidArgument {
		t.Errorf("second voice_close: %d, want %d", e, audio.InvalidArgument)
	}
}

func TestVoiceEventsAndProcessEnd(t *testing.T) {
	prov := &audiotest.Provider{}
	h := newHarness(t, harnessOpts{caps: audioCap, prov: prov, label: "q"})
	v := h.voiceOpen(22050)
	sinks := prov.VoiceSinks()
	if len(sinks) != 1 || sinks[0].Container != "q" || sinks[0].MixRate != 22050 {
		t.Fatalf("sinks %+v", sinks)
	}
	if got := binary.LittleEndian.Uint32(h.m[0:]); got != v {
		t.Fatalf("handle slot %d, want %d", got, v)
	}
	h.p.Close() // the process ends with the mixer open
	if sinks[0].CloseCalls() != 1 {
		t.Errorf("sink closed %d times at process end, want 1", sinks[0].CloseCalls())
	}
	var open, closed int
	for _, e := range *h.events {
		switch e {
		case "wctr_audio_out voice_open q":
			open++
		case "wctr_audio_out voice_close q":
			closed++
		}
	}
	if open != 1 || closed != 1 {
		t.Errorf("events %v: voice_open %d voice_close %d, want 1 and 1", *h.events, open, closed)
	}
}

// The bound of what a worker's SharedArrayBuffer for the bank may be: the page
// cannot be made to hold more than VoicesPerPage * MaxBankBytes.
func TestVoiceBankBound(t *testing.T) {
	if audio.VoicesPerPage*audio.MaxBankBytes > 64<<20 {
		t.Errorf("%d mixers x %d bank bytes exceed the 64 MiB page budget", audio.VoicesPerPage, audio.MaxBankBytes)
	}
}

// A sequence of voice calls decoded from data never panics, answers only the
// documented errnos, and leaves every quota whole once the process ends.
func FuzzVoiceCalls(f *testing.F) {
	f.Add([]byte{0, 0x11, 0x2B, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{4, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		prov := &audiotest.Provider{}
		mod := audio.New(prov)
		p := wasi.New(wasi.Config{FS: rootfs.New(), Modules: []wasi.HostModule{mod}, Caps: audioCap, Label: "fz"})
		m := make(mem, 1<<15)
		const rec = 1 + 8*4
		for k := 0; k+rec <= len(data); k += rec {
			fn := int(data[k] % 9)
			var a [8]uint64
			for j := range a {
				a[j] = uint64(binary.LittleEndian.Uint32(data[k+1+4*j:]))
			}
			args := a[:]
			switch fn {
			case fnOpen, fnWrite:
				args = a[:4]
			case fnStats, fnVoiceOpen, fnVoiceStats:
				args = a[:2]
			case fnClose, fnVoiceClose:
				args = a[:1]
			case fnVoiceCommit:
				args = a[:4]
			}
			if data[k]&0x80 != 0 { // steer toward live handles and small addresses
				args[0] = args[0]%4 + 1
			}
			r := p.CallImport(len(wasi.Funcs)+fn, args, m)
			if r.Trap != "" {
				t.Fatalf("fn %d args %v trapped: %s", fn, args, r.Trap)
			}
			switch r.Ret {
			case audio.Success, audio.InvalidArgument, audio.Unsupported, audio.TooLarge, audio.QuotaExceeded, audio.Fault:
			default:
				t.Fatalf("fn %d args %v: unexpected errno %d", fn, args, r.Ret)
			}
		}
		p.Close()
		for _, s := range prov.VoiceSinks() {
			if s.CloseCalls() != 1 {
				t.Fatalf("a mixer was closed %d times", s.CloseCalls())
			}
		}
		q := wasi.New(wasi.Config{FS: rootfs.New(), Modules: []wasi.HostModule{mod}, Caps: audioCap, Label: "fz"})
		defer q.Close()
		for k := 0; k < audio.StreamsPerContainer; k++ {
			if r := q.CallImport(len(wasi.Funcs)+fnOpen, []uint64{11025, 2, 256, 0}, m); uint32(r.Ret) != audio.Success {
				t.Fatalf("slot %d not returned: errno %d", k, r.Ret)
			}
		}
	})
}
