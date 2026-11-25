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

// HRefused is the header word where the guest's worker counts the frames it
// refused itself (worker.js, audio.OutOfBand).
const HRefused = 7

// Refused implements audio.OutOfBand.
func (r *Ring) Refused() uint32 { return uint32(r.Load(HRefused)) }

// WorkerRefused plays the guest's worker refusing n frames out of band.
func (r *Ring) WorkerRefused(n int) { r.Add(HRefused, int32(n)) }

// CloseFromPage plays a page that drops the ring: the state word flips.
func (r *Ring) CloseFromPage() { r.Store(audioring.HState, audioring.StateClosed) }

// VoiceSink is one mixer opened by OpenVoices, and its audio.VoiceSink.
type VoiceSink struct {
	Container string
	MixRate   int
	mu        sync.Mutex
	closes    int
}

// Close implements audio.VoiceSink.
func (v *VoiceSink) Close() { v.mu.Lock(); v.closes++; v.mu.Unlock() }

// CloseCalls is how many times the module closed the sink (it must be one).
func (v *VoiceSink) CloseCalls() int { v.mu.Lock(); defer v.mu.Unlock(); return v.closes }

// Provider is the audio.Provider and audio.VoiceProvider. Rings lists every
// ring opened, in order.
type Provider struct {
	mu    sync.Mutex
	Rings []*Ring
	voice []*VoiceSink
	// Err, when set, is what Open returns.
	Err error
	// VoiceErr, when set, is what OpenVoices returns.
	VoiceErr error
}

var (
	_ audio.Provider      = (*Provider)(nil)
	_ audio.VoiceProvider = (*Provider)(nil)
)

// OpenVoices implements audio.VoiceProvider.
func (p *Provider) OpenVoices(container string, mixRate int) (audio.VoiceSink, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.VoiceErr != nil {
		return nil, p.VoiceErr
	}
	v := &VoiceSink{Container: container, MixRate: mixRate}
	p.voice = append(p.voice, v)
	return v, nil
}

// VoiceSinks lists every mixer opened, in order.
func (p *Provider) VoiceSinks() []*VoiceSink {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]*VoiceSink(nil), p.voice...)
}

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
