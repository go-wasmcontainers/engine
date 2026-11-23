//go:build js && wasm

package audio

import (
	"sync"
	"syscall/js"
	"unsafe"

	"github.com/wasmdesk/wasmbox/audioring"
)

// Header words the guest's worker (runtime/browser/worker.js) and this provider
// share beside audioring's own (0..6).
const (
	// hRefused counts the frames the worker refused itself (OutOfBand).
	hRefused = 7
	// hAdopted is 1 when the ring is the buffer the worker allocated: the
	// worker then serves stream_write and stream_stats for it, on its own thread.
	hAdopted = 8
)

// RingsGlobal is the engine worker's global where runtime/browser parks the
// ring a guest's worker allocated for its next stream_open, keyed by container
// label. Open takes it (once) instead of allocating its own.
const RingsGlobal = "__wctrAudioRings"

// Browser is the Provider of the engine worker (section 5): each stream is a
// SharedArrayBuffer ring, announced to the page with a message the page turns
// into an AudioWorkletNode.
//
//	open:  {t:"audio", op:"open", stream, container, sampleRate, channels, capacityFrames, sab}
//	close: {t:"audio", op:"close", stream}
//
// The ring is allocated by the guest's worker when it can (worker.js posts it
// ahead of stream_open; the guest's worker is blocked in Atomics.wait while the
// engine serves a call, so it cannot be handed a buffer afterwards): the worker
// then writes the samples itself and a write never waits for this thread, which
// the garbage collector and every other host call share.
type Browser struct {
	// Post sends a message to the page (the engine worker's postMessage).
	Post func(msg js.Value)

	mu   sync.Mutex
	next int
}

var _ Provider = (*Browser)(nil)

// Open implements Provider.
func (b *Browser) Open(container string, sampleRate, channels, capacityFrames int) (Sink, error) {
	if err := audioring.Validate(sampleRate, channels, capacityFrames); err != nil {
		return nil, err
	}
	b.mu.Lock()
	b.next++
	id := b.next
	b.mu.Unlock()
	g := js.Global()
	size := audioring.SABBytes(channels, capacityFrames)
	buf := takeRing(container, size)
	adopted := buf.Truthy()
	if !adopted {
		buf = g.Get("SharedArrayBuffer").New(size)
	}
	m := &sab{
		atomics: g.Get("Atomics"),
		hdr:     g.Get("Int32Array").New(buf, 0, audioring.HeaderBytes/4),
		bytes:   g.Get("Uint8Array").New(buf),
	}
	if adopted {
		m.Store(hAdopted, 1)
	}
	w, err := audioring.New(m, sampleRate, channels, capacityFrames)
	if err != nil {
		return nil, err
	}
	msg := js.ValueOf(map[string]any{
		"t": "audio", "op": "open", "stream": id, "container": container,
		"sampleRate": sampleRate, "channels": channels, "capacityFrames": capacityFrames,
	})
	msg.Set("sab", buf)
	b.Post(msg)
	return &browserSink{b: b, id: id, w: w, m: m}, nil
}

// takeRing returns the SharedArrayBuffer the container's worker parked for this
// open if it is the size the stream needs, and forgets it; undefined otherwise.
func takeRing(container string, size int) js.Value {
	rings := js.Global().Get(RingsGlobal)
	if !rings.Truthy() {
		return js.Undefined()
	}
	buf := rings.Get(container)
	if !buf.Truthy() {
		return js.Undefined()
	}
	rings.Delete(container)
	if buf.Get("byteLength").Int() != size || !buf.InstanceOf(js.Global().Get("SharedArrayBuffer")) {
		return js.Undefined()
	}
	return buf
}

// sab is the audioring.Backing over a SharedArrayBuffer.
type sab struct {
	atomics js.Value // the Atomics global
	hdr     js.Value // Int32Array over the header
	bytes   js.Value // Uint8Array over the whole buffer
}

func (s *sab) Load(i int) int32     { return int32(s.atomics.Call("load", s.hdr, i).Int()) }
func (s *sab) Store(i int, v int32) { s.atomics.Call("store", s.hdr, i, v) }
func (s *sab) Add(i int, d int32)   { s.atomics.Call("add", s.hdr, i, d) }

func (s *sab) WriteSamples(at int, src []float32) {
	b := unsafe.Slice((*byte)(unsafe.Pointer(&src[0])), len(src)*4)
	off := audioring.HeaderBytes + at*4
	js.CopyBytesToJS(s.bytes.Call("subarray", off, off+len(b)), b)
}

type browserSink struct {
	b  *Browser
	id int
	w  *audioring.Writer
	m  *sab
}

func (s *browserSink) Writer() *audioring.Writer { return s.w }

// Refused implements OutOfBand.
func (s *browserSink) Refused() uint32 { return uint32(s.m.Load(hRefused)) }

// Close stores the ring's closed state, which stops the worklet, and tells the
// page to drop the node.
func (s *browserSink) Close() {
	s.w.Close()
	s.b.Post(js.ValueOf(map[string]any{"t": "audio", "op": "close", "stream": s.id}))
}
