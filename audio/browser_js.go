//go:build js && wasm

package audio

import (
	"sync"
	"syscall/js"

	"github.com/wasmdesk/wasmbox/audioring"
)

// Browser is the Provider of the engine worker (section 5): each stream is a
// SharedArrayBuffer ring, announced to the page with a message the page turns
// into an AudioWorkletNode.
//
//	open:  {t:"audio", op:"open", stream, container, sampleRate, channels, capacityFrames, sab}
//	close: {t:"audio", op:"close", stream}
type Browser struct {
	// Post sends a message to the page (the engine worker's postMessage).
	Post func(msg js.Value)

	mu   sync.Mutex
	next int
}

var _ Provider = (*Browser)(nil)

// Open implements Provider.
func (b *Browser) Open(container string, sampleRate, channels, capacityFrames int) (Sink, error) {
	b.mu.Lock()
	b.next++
	id := b.next
	b.mu.Unlock()
	// audioring.Open allocates the buffer and would post its own compositor
	// message; here the page gets this module's message instead.
	w, buf, err := audioring.Open(audioring.Options{
		SampleRate: sampleRate, Channels: channels, CapacityFrames: capacityFrames,
		Post: func(js.Value) {},
	})
	if err != nil {
		return nil, err
	}
	msg := js.ValueOf(map[string]any{
		"t": "audio", "op": "open", "stream": id, "container": container,
		"sampleRate": sampleRate, "channels": channels, "capacityFrames": capacityFrames,
	})
	msg.Set("sab", buf)
	b.Post(msg)
	return &browserSink{b: b, id: id, w: w}, nil
}

type browserSink struct {
	b  *Browser
	id int
	w  *audioring.Writer
}

func (s *browserSink) Writer() *audioring.Writer { return s.w }

// Close stores the ring's closed state, which stops the worklet, and tells the
// page to drop the node.
func (s *browserSink) Close() {
	s.w.Close()
	s.b.Post(js.ValueOf(map[string]any{"t": "audio", "op": "close", "stream": s.id}))
}
