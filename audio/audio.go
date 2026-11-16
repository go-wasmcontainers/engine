// Package audio is the host module wctr_audio_out: a container granted
// `run --audio` writes PCM into a stream, and the page plays it through an
// AudioWorklet. docs/audio-design.md is the specification; section numbers
// below refer to it.
//
// The module is platform neutral. Where the samples go is a Provider: in the
// browser, Browser (browser_js.go) allocates a SharedArrayBuffer ring per
// stream and tells the page; natively there is no provider and every call
// answers Unsupported. The module owns what is not platform specific: the
// grant, the quotas, the range checks, the sanitising of samples, the
// "waiting" state before the page plays, and the events.
package audio

import (
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"sync"
	"time"

	"github.com/go-wasmcontainers/engine/wasi"
	"github.com/wasmdesk/wasmbox/audioring"
)

// ModuleName is the import module name. It is not a wasi_ name: no WASI
// proposal backs it (section 3).
const ModuleName = "wctr_audio_out"

// Capability is the device capability that grants the module (run --audio).
const Capability = "audio"

// Errno values of the module (section 3). Success is 0; the numbering is
// shared with wasi_webgpu_batch where the meaning is the same.
const (
	Success         = 0
	InvalidArgument = 1
	Denied          = 2
	Unsupported     = 3
	TooLarge        = 4
	QuotaExceeded   = 5
	Fault           = 9
)

// Stream states reported by stream_stats.
const (
	StateWaiting = 0 // the page does not play: writes are accepted and discarded
	StatePlaying = 1
	StateClosed  = 2 // closed by the host (page gone, ring closed)
)

// Bounds (section 4).
const (
	MinRate     = 8000
	MaxRate     = 48000
	MinCapacity = 256
	MaxCapacity = 16384 // frames; a power of two

	StreamsPerContainer = 2
	StreamsPerPage      = 8

	// HeartbeatStale is how long the worklet's heartbeat may stand still before
	// a stream goes back to waiting.
	HeartbeatStale = 200 * time.Millisecond

	// MaxCopy is the most bytes one stream_write copies: MaxCapacity stereo
	// float32 frames.
	MaxCopy = MaxCapacity * 2 * 4

	// refusedEventEvery spaces the "refused" events of one stream, so a guest
	// that outruns the page cannot flood the event bus.
	refusedEventEvery = time.Second
)

// ErrQuota is what a Provider returns when the page cannot take one more
// stream; the module answers QuotaExceeded.
var ErrQuota = errors.New("audio: too many streams")

// Sink is one open stream's ring, as the provider made it.
type Sink interface {
	// Writer is the producer half of the ring. The module is its only writer.
	Writer() *audioring.Writer
	// Close closes the ring and tells the page. Called once.
	Close()
}

// Provider opens rings. The module has validated the arguments (section 4)
// before it calls.
type Provider interface {
	Open(container string, sampleRate, channels, capacityFrames int) (Sink, error)
}

// Module is the host module wctr_audio_out. One value serves every container
// of an engine: it carries the page-wide stream quota.
type Module struct {
	// Provider receives the streams. Nil answers Unsupported (native wctr).
	Provider Provider
	// Now is the clock the heartbeat age is measured on; nil is time.Now.
	Now func() time.Time

	mu           sync.Mutex
	page         int
	perContainer map[string]int
}

// New returns the module over p (nil: every call answers Unsupported).
func New(p Provider) *Module { return &Module{Provider: p} }

var _ wasi.HostModule = (*Module)(nil)

// Module implements wasi.HostModule.
func (*Module) Module() string { return ModuleName }

// Function indexes of Funcs.
const (
	fnOpen = iota
	fnWrite
	fnStats
	fnClose
)

// Funcs implements wasi.HostModule.
func (*Module) Funcs() []wasi.HostFunc {
	return []wasi.HostFunc{
		fnOpen:  {Name: "stream_open", Params: "iiii", Result: "i"},
		fnWrite: {Name: "stream_write", Params: "iiii", Result: "i"},
		fnStats: {Name: "stream_stats", Params: "ii", Result: "i"},
		fnClose: {Name: "stream_close", Params: "i", Result: "i"},
	}
}

// Open implements wasi.HostModule.
func (m *Module) Open(p *wasi.Proc) wasi.Instance {
	return &instance{m: m, p: p, label: p.Label(), granted: p.HasCap(Capability), streams: map[uint32]*stream{}, next: 1}
}

func (m *Module) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// acquire reserves a stream slot for the container and the page.
func (m *Module) acquire(label string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.page >= StreamsPerPage || m.perContainer[label] >= StreamsPerContainer {
		return false
	}
	if m.perContainer == nil {
		m.perContainer = map[string]int{}
	}
	m.page++
	m.perContainer[label]++
	return true
}

func (m *Module) release(label string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.page--
	if m.perContainer[label]--; m.perContainer[label] <= 0 {
		delete(m.perContainer, label)
	}
}

type stream struct {
	sink              Sink
	w                 *audioring.Writer
	rate              int
	channels, cap     int
	scratch           []float32
	refused           uint32
	lastHB            uint32
	lastHBAt          time.Time
	lastRefusedEvent  time.Time
	refusedUnreported uint32
}

// state is the stream's state at now (section 3): closed if the ring is, playing
// while the worklet's heartbeat moves, waiting otherwise.
func (s *stream) state(now time.Time) uint32 {
	if s.w.Closed() {
		return StateClosed
	}
	if hb := s.w.Heartbeat(); hb != s.lastHB {
		s.lastHB, s.lastHBAt = hb, now
	}
	if s.lastHBAt.IsZero() || now.Sub(s.lastHBAt) > HeartbeatStale {
		return StateWaiting
	}
	return StatePlaying
}

type instance struct {
	m       *Module
	p       *wasi.Proc
	label   string
	granted bool

	mu      sync.Mutex
	closed  bool
	next    uint32
	streams map[uint32]*stream
}

func errno(e uint32) wasi.Result { return wasi.Result{Ret: uint64(e)} }

// readRange is Memory.Read with the range computed in 64 bits: ptr and n are
// guest values, and a sum that wraps at 2^32 must not reach an executor that
// adds them in 32.
func readRange(m wasi.Memory, ptr, n uint32) ([]byte, bool) {
	if uint64(ptr)+uint64(n) > 1<<32 {
		return nil, false
	}
	return m.Read(ptr, n)
}

func writeRange(m wasi.Memory, ptr uint32, b []byte) bool {
	if uint64(ptr)+uint64(len(b)) > 1<<32 {
		return false
	}
	return m.Write(ptr, b)
}

// Call implements wasi.Instance.
func (i *instance) Call(fn int, a []uint64, m wasi.Memory) wasi.Result {
	i.mu.Lock()
	defer i.mu.Unlock()
	switch {
	case !i.granted:
		return errno(Denied)
	case i.m.Provider == nil:
		return errno(Unsupported)
	case i.closed:
		return errno(InvalidArgument)
	}
	arg := func(k int) uint32 { return uint32(a[k]) }
	switch fn {
	case fnOpen:
		return errno(i.open(arg(0), arg(1), arg(2), arg(3), m))
	case fnWrite:
		return errno(i.write(arg(0), arg(1), arg(2), arg(3), m))
	case fnStats:
		return errno(i.stats(arg(0), arg(1), m))
	case fnClose:
		s, ok := i.streams[arg(0)]
		if !ok {
			return errno(InvalidArgument)
		}
		i.drop(arg(0), s)
		return errno(Success)
	}
	return wasi.Result{Trap: "wctr_audio_out: unknown function"}
}

func (i *instance) open(rate, channels, capacity, out uint32, m wasi.Memory) uint32 {
	if rate < MinRate || rate > MaxRate || channels < 1 || channels > 2 ||
		capacity < MinCapacity || capacity > MaxCapacity || capacity&(capacity-1) != 0 {
		return InvalidArgument
	}
	// The handle's slot is checked before anything is allocated, so a Fault
	// never leaves a stream behind.
	if _, ok := readRange(m, out, 4); !ok {
		return Fault
	}
	if !i.m.acquire(i.label) {
		return QuotaExceeded
	}
	sink, err := i.m.Provider.Open(i.label, int(rate), int(channels), int(capacity))
	if err != nil {
		i.m.release(i.label)
		if errors.Is(err, ErrQuota) {
			return QuotaExceeded
		}
		return Unsupported
	}
	h := i.next
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], h)
	if !writeRange(m, out, b[:]) {
		sink.Close()
		i.m.release(i.label)
		return Fault
	}
	i.next++
	i.streams[h] = &stream{sink: sink, w: sink.Writer(), rate: int(rate), channels: int(channels), cap: int(capacity)}
	i.p.Emit(ModuleName, "open", map[string]string{
		"container": i.label, "stream": strconv.FormatUint(uint64(h), 10),
		"rate": strconv.Itoa(int(rate)), "channels": strconv.Itoa(int(channels)), "capacity": strconv.Itoa(int(capacity)),
	})
	return Success
}

func (i *instance) write(h, ptr, frames, out uint32, m wasi.Memory) uint32 {
	s, ok := i.streams[h]
	if !ok {
		return InvalidArgument
	}
	if frames > uint32(s.cap) {
		return TooLarge
	}
	if _, ok := readRange(m, out, 8); !ok {
		return Fault
	}
	now := i.m.now()
	state := s.state(now)
	var accepted, queued uint32
	if frames > 0 {
		n := frames * uint32(s.channels) * 4 // at most MaxCopy
		raw, ok := readRange(m, ptr, n)
		if !ok || uint32(len(raw)) != n {
			return Fault
		}
		switch state {
		case StatePlaying:
			if s.scratch == nil {
				s.scratch = make([]float32, s.cap*s.channels)
			}
			pcm := s.scratch[:frames*uint32(s.channels)]
			for k := range pcm {
				pcm[k] = sanitize(math.Float32frombits(binary.LittleEndian.Uint32(raw[4*k:])))
			}
			accepted = uint32(s.w.Write(pcm))
			if accepted < frames {
				i.refuse(s, frames-accepted, now)
			}
		case StateWaiting:
			// Sound made before the page plays is never played late: accepted
			// (the guest must not retry it) and dropped.
			accepted = frames
		default:
			i.refuse(s, frames, now)
		}
	}
	if state == StatePlaying {
		queued = uint32(s.w.Buffered())
	}
	var rec [8]byte
	binary.LittleEndian.PutUint32(rec[0:], accepted)
	binary.LittleEndian.PutUint32(rec[4:], queued)
	if !writeRange(m, out, rec[:]) {
		return Fault
	}
	return Success
}

func (i *instance) stats(h, out uint32, m wasi.Memory) uint32 {
	s, ok := i.streams[h]
	if !ok {
		return InvalidArgument
	}
	if _, ok := readRange(m, out, 16); !ok {
		return Fault
	}
	st := s.w.Stats()
	var rec [16]byte
	binary.LittleEndian.PutUint32(rec[0:], st.Played)
	binary.LittleEndian.PutUint32(rec[4:], st.Underruns)
	binary.LittleEndian.PutUint32(rec[8:], s.refused)
	binary.LittleEndian.PutUint32(rec[12:], s.state(i.m.now()))
	if !writeRange(m, out, rec[:]) {
		return Fault
	}
	return Success
}

// refuse counts frames the guest must retry or drop, and reports them at most
// once per refusedEventEvery.
func (i *instance) refuse(s *stream, frames uint32, now time.Time) {
	s.refused += frames
	s.refusedUnreported += frames
	if now.Sub(s.lastRefusedEvent) < refusedEventEvery {
		return
	}
	s.lastRefusedEvent = now
	i.p.Emit(ModuleName, "refused", map[string]string{
		"container": i.label, "frames": strconv.FormatUint(uint64(s.refusedUnreported), 10),
	})
	s.refusedUnreported = 0
}

// drop closes one stream and gives its slot back. i.mu is held.
func (i *instance) drop(h uint32, s *stream) {
	delete(i.streams, h)
	s.sink.Close()
	i.m.release(i.label)
	i.p.Emit(ModuleName, "close", map[string]string{"container": i.label, "stream": strconv.FormatUint(uint64(h), 10)})
}

// Close implements wasi.Instance: every stream of the process is closed (ring
// closed, page told) and later calls fail.
func (i *instance) Close() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.closed {
		return
	}
	i.closed = true
	for h, s := range i.streams {
		i.drop(h, s)
	}
}

// sanitize is the copy's filter (section 3): a non-finite sample becomes 0 and
// every sample is clamped to [-1, 1]. A guest cannot make the page play
// anything louder than full scale, and no NaN reaches the audio thread.
func sanitize(v float32) float32 {
	switch {
	case v != v || v > math.MaxFloat32 || v < -math.MaxFloat32:
		return 0
	case v > 1:
		return 1
	case v < -1:
		return -1
	}
	return v
}
