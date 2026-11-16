// Package audiotest is an in-memory audio.Provider for tests: its rings are
// audioring Backings over plain memory, and the test plays the page by calling
// Consume and Beat.
package audiotest

import (
	"sync"

	"github.com/go-wasmcontainers/engine/audio"
	"github.com/wasmdesk/wasmbox/audioring"
)

// Ring is one stream's ring, and its audio.Sink.
type Ring struct {
	Container                string
	Rate, Channels, Capacity int
	mu                       sync.Mutex
	hdr                      [16]int32
	data                     []float32
	w                        *audioring.Writer
	closeCalls               int
}

// Load implements audioring.Backing.
func (r *Ring) Load(i int) int32 { r.mu.Lock(); defer r.mu.Unlock(); return r.hdr[i] }

// Store implements audioring.Backing.
func (r *Ring) Store(i int, v int32) { r.mu.Lock(); defer r.mu.Unlock(); r.hdr[i] = v }

// Add implements audioring.Backing.
func (r *Ring) Add(i int, d int32) { r.mu.Lock(); defer r.mu.Unlock(); r.hdr[i] += d }

// WriteSamples implements audioring.Backing.
func (r *Ring) WriteSamples(at int, src []float32) {
	r.mu.Lock()
	defer r.mu.Unlock()
	copy(r.data[at:], src)
}

// Writer implements audio.Sink.
func (r *Ring) Writer() *audioring.Writer { return r.w }

// Close implements audio.Sink: it closes the ring, as the browser provider does.
func (r *Ring) Close() {
	r.mu.Lock()
	r.closeCalls++
	r.mu.Unlock()
	r.w.Close()
}

// CloseCalls is how many times the module closed the sink (it must be one).
func (r *Ring) CloseCalls() int { r.mu.Lock(); defer r.mu.Unlock(); return r.closeCalls }

// Beat plays the worklet's heartbeat: the consumer ran one more quantum.
func (r *Ring) Beat() { r.Add(audioring.HBeat, 1) }

// Consume plays the worklet's read side: it takes up to n frames, advances the
// read index and the played counter, and returns the interleaved samples.
func (r *Ring) Consume(n int) []float32 {
	r.mu.Lock()
	defer r.mu.Unlock()
	rd := uint32(r.hdr[audioring.HRead])
	avail := int(audioring.Buffered(uint32(r.hdr[audioring.HWrite]), rd))
	if n > avail {
		n = avail
	}
	out := make([]float32, 0, n*r.Channels)
	for k := 0; k < n; k++ {
		base := int((rd+uint32(k))&uint32(r.Capacity-1)) * r.Channels
		out = append(out, r.data[base:base+r.Channels]...)
	}
	r.hdr[audioring.HRead] = int32(rd + uint32(n))
	r.hdr[audioring.HPlayed] += int32(n)
	return out
}

// CloseFromPage plays a page that drops the ring: the state word flips.
func (r *Ring) CloseFromPage() { r.Store(audioring.HState, audioring.StateClosed) }

// Provider is the audio.Provider. Rings lists every ring opened, in order.
type Provider struct {
	mu    sync.Mutex
	Rings []*Ring
	// Err, when set, is what Open returns.
	Err error
}

var _ audio.Provider = (*Provider)(nil)

// Open implements audio.Provider.
func (p *Provider) Open(container string, rate, channels, capacity int) (audio.Sink, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return nil, p.Err
	}
	r := &Ring{Container: container, Rate: rate, Channels: channels, Capacity: capacity, data: make([]float32, channels*capacity)}
	w, err := audioring.New(r, rate, channels, capacity)
	if err != nil {
		return nil, err
	}
	r.w = w
	p.Rings = append(p.Rings, r)
	return r, nil
}

// Last is the most recently opened ring, or nil.
func (p *Provider) Last() *Ring {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.Rings) == 0 {
		return nil
	}
	return p.Rings[len(p.Rings)-1]
}
