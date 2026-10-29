package container

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Restart policies (Docker's HostConfig.RestartPolicy, run --restart):
//
//   - no (or empty): never restarted;
//   - always: restarted whenever it exits, unless stopped or killed by a
//     user (docker stop / kill), and started again when the engine starts;
//   - unless-stopped: the same, but not started by the engine if a user had
//     stopped it;
//   - on-failure[:N]: restarted when it exits with a code other than 0, at
//     most N times (0: no bound).
//
// A restart waits first, as Docker's restart manager does: 100 ms, doubled
// after each run shorter than RestartReset, at most RestartMaxDelay; a run
// that lasted RestartReset or longer starts the delay over. A start the
// engine refuses (a volume, a network, a port) is not retried.

// RestartPolicy is a container's restart policy.
type RestartPolicy struct {
	Name              string `json:"name,omitempty"`
	MaximumRetryCount int    `json:"maximumRetryCount,omitempty"`
}

// Delays between automatic restarts.
const (
	RestartFirstDelay = 100 * time.Millisecond
	RestartMaxDelay   = time.Minute
	RestartReset      = 10 * time.Second
	// MaxRetryCount bounds on-failure:N.
	MaxRetryCount = 1 << 20
)

// Restarting is the state of a container waiting for its automatic
// restart.
const Restarting State = "restarting"

// ParseRestartPolicy reads run --restart: no, always, unless-stopped,
// on-failure or on-failure:N.
func ParseRestartPolicy(s string) (RestartPolicy, error) {
	name, n, hasN := strings.Cut(s, ":")
	p := RestartPolicy{Name: name}
	if hasN {
		if name != "on-failure" {
			return p, fmt.Errorf("%w: restart policy %q: only on-failure takes a count", ErrInvalid, s)
		}
		v, err := strconv.Atoi(n)
		if err != nil {
			return p, fmt.Errorf("%w: restart policy %q: the count is not a number", ErrInvalid, s)
		}
		p.MaximumRetryCount = v
	}
	return p, p.check()
}

func (p RestartPolicy) check() error {
	switch p.Name {
	case "", "no", "always", "unless-stopped":
		if p.MaximumRetryCount != 0 {
			return fmt.Errorf("%w: maximum retry count cannot be used with restart policy %q", ErrInvalid, p.Name)
		}
	case "on-failure":
		if p.MaximumRetryCount < 0 || p.MaximumRetryCount > MaxRetryCount {
			return fmt.Errorf("%w: maximum retry count must be 0 to %d", ErrInvalid, MaxRetryCount)
		}
	default:
		return fmt.Errorf("%w: invalid restart policy %q (no, always, unless-stopped, on-failure[:N])", ErrInvalid, p.Name)
	}
	return nil
}

// storedState is the state a record keeps: a container waiting for its
// restart is stored as exited (the next engine's AutoStart decides).
func storedState(s State) State {
	if s == Restarting {
		return Exited
	}
	return s
}

// None reports a policy that never restarts.
func (p RestartPolicy) None() bool { return p.Name == "" || p.Name == "no" }

// checkRestart validates a container's policy at create.
func checkRestart(cfg Config) error {
	if err := cfg.Restart.check(); err != nil {
		return err
	}
	if cfg.AutoRemove && !cfg.Restart.None() {
		return fmt.Errorf("%w: conflicting options: cannot specify both --restart and --rm", ErrInvalid)
	}
	return nil
}

// shouldRestart reports whether c, which has just exited after running,
// restarts under its policy. The caller holds c.mu.
func (m *Manager) shouldRestartLocked(c *Container) bool {
	if c.manualStop || c.removed || m.stopping.Load() {
		return false
	}
	switch p := c.cfg.Restart; p.Name {
	case "always", "unless-stopped":
		return true
	case "on-failure":
		return c.exitCode != 0 && (p.MaximumRetryCount == 0 || c.restartCount < p.MaximumRetryCount)
	}
	return false
}

// scheduleRestartLocked moves c to Restarting and starts it again after
// its delay. The caller holds c.mu.
func (m *Manager) scheduleRestartLocked(c *Container) {
	ran := c.finishedAt.Sub(c.startedAt)
	switch {
	case c.restartDelay == 0 || ran >= RestartReset:
		c.restartDelay = RestartFirstDelay
	default:
		c.restartDelay = min(2*c.restartDelay, RestartMaxDelay)
	}
	c.state = Restarting
	c.notifyLocked()
	c.restartTimer = time.AfterFunc(c.restartDelay, func() { m.autoRestart(c) })
}

// autoRestart is the restart a policy asked for, once its delay is over.
func (m *Manager) autoRestart(c *Container) {
	c.mu.Lock()
	if c.state != Restarting || c.removed || m.stopping.Load() {
		c.mu.Unlock()
		return
	}
	c.restartTimer = nil
	c.restartCount++
	c.state = Exited // what start expects; it goes to Running at once
	c.mu.Unlock()
	m.start(context.Background(), c, true)
}

// cancelRestartLocked stops a pending automatic restart: the container is
// Exited again. It reports whether one was pending. The caller holds c.mu.
func (c *Container) cancelRestartLocked() bool {
	if c.state != Restarting {
		return false
	}
	if c.restartTimer != nil {
		c.restartTimer.Stop()
		c.restartTimer = nil
	}
	c.state = Exited
	c.notifyLocked()
	return true
}

// stopByUser marks c stopped by a user (stop, kill): its policy no longer
// restarts it, and a pending restart is cancelled. It reports whether a
// restart was pending.
func (c *Container) stopByUser() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manualStop = true
	return c.cancelRestartLocked()
}

// AutoStart starts, when the engine starts, the containers whose policy
// says so: always, and unless-stopped unless a user had stopped it.
func (m *Manager) AutoStart(ctx context.Context) []error {
	m.mu.Lock()
	cs := make([]*Container, 0, len(m.byID))
	for _, c := range m.byID {
		cs = append(cs, c)
	}
	m.mu.Unlock()
	var errs []error
	for _, c := range cs {
		c.mu.Lock()
		p, stopped, state := c.cfg.Restart, c.manualStop, c.state
		c.mu.Unlock()
		if state == Running || !(p.Name == "always" || (p.Name == "unless-stopped" && !stopped)) {
			continue
		}
		if err := m.Start(ctx, c.id); err != nil {
			errs = append(errs, fmt.Errorf("container %s (%s): %w", c.id[:12], c.name, err))
		}
	}
	return errs
}
