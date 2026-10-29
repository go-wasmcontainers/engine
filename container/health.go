package container

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Health checks (Docker's Config.Healthcheck, run --health-*): a command
// run in the container at an interval, through the exec machinery (so in
// its process group, on its trees and network), whose exit code says
// whether the container is healthy.
//
//   - Test is ["CMD", arg...], ["CMD-SHELL", command] (run by /bin/sh -c),
//     or ["NONE"] (no check, overriding the image's).
//   - Until StartPeriod has passed, a probe runs every StartInterval and a
//     failure does not count; a success makes the container healthy at once.
//   - After it, a probe runs every Interval; Retries consecutive failures
//     make it unhealthy, a success healthy again.
//   - A probe longer than Timeout is killed and counts as a failure.
//
// The state is "starting" until a status is reached; the last
// HealthLogSize probes are kept with at most HealthOutputBytes of their
// output. A change of status is a "health_status: <status>" event. The
// probes stop with the container.

// Health statuses.
const (
	HealthNone      = "none"
	HealthStarting  = "starting"
	HealthHealthy   = "healthy"
	HealthUnhealthy = "unhealthy"
)

// Defaults and bounds of a health check (Docker's defaults; the minimum
// interval is this engine's: each probe is a process to compile and run).
const (
	DefaultHealthInterval      = 30 * time.Second
	DefaultHealthTimeout       = 30 * time.Second
	DefaultHealthStartInterval = 5 * time.Second
	DefaultHealthRetries       = 3
	MinHealthInterval          = 100 * time.Millisecond
	MaxHealthRetries           = 1 << 16
	HealthLogSize              = 5
	HealthOutputBytes          = 4096
	maxHealthTest              = 64 << 10
)

// HealthConfig is a container's health check.
type HealthConfig struct {
	Test          []string      `json:"test,omitempty"`
	Interval      time.Duration `json:"interval,omitempty"`
	Timeout       time.Duration `json:"timeout,omitempty"`
	StartPeriod   time.Duration `json:"startPeriod,omitempty"`
	StartInterval time.Duration `json:"startInterval,omitempty"`
	Retries       int           `json:"retries,omitempty"`
}

// HealthResult is one probe.
type HealthResult struct {
	Start, End time.Time
	ExitCode   int
	Output     string
}

// Health is a container's health state (inspect's State.Health).
type Health struct {
	Status        string
	FailingStreak int
	Log           []HealthResult
}

// Enabled reports a check that runs (a test other than NONE).
func (h *HealthConfig) Enabled() bool {
	return h != nil && len(h.Test) > 0 && h.Test[0] != "NONE"
}

// checkHealth validates a container's health check.
func checkHealth(h *HealthConfig) error {
	if h == nil || len(h.Test) == 0 {
		return nil
	}
	n := 0
	for _, s := range h.Test {
		n += len(s)
	}
	if n > maxHealthTest {
		return fmt.Errorf("%w: health check test longer than %d bytes", ErrInvalid, maxHealthTest)
	}
	switch h.Test[0] {
	case "NONE":
		if len(h.Test) != 1 {
			return fmt.Errorf("%w: health check NONE takes no arguments", ErrInvalid)
		}
	case "CMD":
		if len(h.Test) < 2 || h.Test[1] == "" {
			return fmt.Errorf("%w: health check CMD needs a command", ErrInvalid)
		}
	case "CMD-SHELL":
		if len(h.Test) != 2 || strings.TrimSpace(h.Test[1]) == "" {
			return fmt.Errorf("%w: health check CMD-SHELL takes one command line", ErrInvalid)
		}
	default:
		return fmt.Errorf("%w: health check test must start with CMD, CMD-SHELL or NONE, not %q", ErrInvalid, h.Test[0])
	}
	for _, d := range []struct {
		name string
		v    time.Duration
	}{{"interval", h.Interval}, {"timeout", h.Timeout}, {"start interval", h.StartInterval}} {
		if d.v != 0 && d.v < MinHealthInterval {
			return fmt.Errorf("%w: health check %s must be at least %v", ErrInvalid, d.name, MinHealthInterval)
		}
	}
	if h.StartPeriod < 0 {
		return fmt.Errorf("%w: health check start period must not be negative", ErrInvalid)
	}
	if h.Retries < 0 || h.Retries > MaxHealthRetries {
		return fmt.Errorf("%w: health check retries must be 0 to %d", ErrInvalid, MaxHealthRetries)
	}
	return nil
}

func or(v, d time.Duration) time.Duration {
	if v == 0 {
		return d
	}
	return v
}

// argv is the probe's command.
func (h *HealthConfig) argv() []string {
	if h.Test[0] == "CMD-SHELL" {
		return []string{"/bin/sh", "-c", h.Test[1]}
	}
	return append([]string(nil), h.Test[1:]...)
}

// healthMonitor runs a running container's probes until ctx ends.
func (m *Manager) healthMonitor(ctx context.Context, c *Container, h HealthConfig) {
	started := time.Now()
	interval, timeout := or(h.Interval, DefaultHealthInterval), or(h.Timeout, DefaultHealthTimeout)
	startInterval := or(h.StartInterval, DefaultHealthStartInterval)
	retries := h.Retries
	if retries == 0 {
		retries = DefaultHealthRetries
	}
	healthy := false
	for {
		inStart := !healthy && time.Since(started) < h.StartPeriod
		wait := interval
		if inStart {
			wait = startInterval
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		res := m.probe(ctx, c, h, timeout)
		if ctx.Err() != nil {
			return
		}
		ok := res.ExitCode == 0
		if ok {
			healthy = true
		}
		inStart = !healthy && time.Since(started) < h.StartPeriod
		m.recordHealth(c, res, ok, inStart, retries)
	}
}

// recordHealth adds a probe's result and moves the status; a change is
// an event.
func (m *Manager) recordHealth(c *Container, res HealthResult, ok, inStart bool, retries int) {
	c.mu.Lock()
	hs := c.health
	if hs == nil {
		c.mu.Unlock()
		return
	}
	hs.Log = append(hs.Log, res)
	if len(hs.Log) > HealthLogSize {
		hs.Log = append([]HealthResult(nil), hs.Log[len(hs.Log)-HealthLogSize:]...)
	}
	old := hs.Status
	switch {
	case ok:
		hs.FailingStreak = 0
		hs.Status = HealthHealthy
	case inStart:
		// a failure in the start period does not count
	default:
		hs.FailingStreak++
		if hs.FailingStreak >= retries {
			hs.Status = HealthUnhealthy
		}
	}
	changed := hs.Status != old
	status := hs.Status
	c.notifyLocked()
	c.mu.Unlock()
	if changed {
		m.emit("health_status: "+status, c, nil)
	}
}

// boundedBuffer keeps the first n bytes written to it.
type boundedBuffer struct {
	mu  sync.Mutex
	n   int
	buf bytes.Buffer
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.n - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// probe runs one check in the container through the exec machinery (not
// listed among the exec instances), killed after timeout.
func (m *Manager) probe(ctx context.Context, c *Container, h HealthConfig, timeout time.Duration) HealthResult {
	res := HealthResult{Start: time.Now().UTC()}
	out := &boundedBuffer{n: HealthOutputBytes}
	x := &Exec{ID: randomHex(32), ContainerID: c.id, created: time.Now(), done: make(chan struct{}),
		Config: ExecConfig{Cmd: h.argv(), AttachStdout: true, AttachStderr: true}}
	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	err := m.startExec(pctx, x, ExecIO{Stdout: out, Stderr: out}, false)
	if err == nil {
		select {
		case <-x.Done():
		case <-pctx.Done():
			x.Kill()
			<-x.Done()
		}
	}
	res.End = time.Now().UTC()
	switch {
	case err != nil:
		res.ExitCode, res.Output = -1, err.Error()
	case pctx.Err() == context.DeadlineExceeded:
		res.ExitCode, res.Output = -1, fmt.Sprintf("Health check exceeded timeout (%v)", timeout)
	default:
		res.ExitCode, res.Output = x.Info().ExitCode, out.String()
	}
	return res
}

// startHealthLocked starts the probes of a container that has just started
// running; the caller holds c.mu. It returns how to stop them.
func (m *Manager) startHealthLocked(c *Container) func() {
	h := c.cfg.Healthcheck
	if !h.Enabled() {
		c.health = nil
		return func() {}
	}
	c.health = &Health{Status: HealthStarting}
	ctx, cancel := context.WithCancel(context.Background())
	go m.healthMonitor(ctx, c, *h)
	return cancel
}

// healthLocked is a copy of c's health state, nil without a check; the
// caller holds c.mu.
func (c *Container) healthLocked() *Health {
	if c.health == nil {
		return nil
	}
	h := *c.health
	h.Log = append([]HealthResult(nil), h.Log...)
	return &h
}
