package container

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func until(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParseRestartPolicy(t *testing.T) {
	for _, c := range []struct {
		in   string
		want RestartPolicy
		ok   bool
	}{
		{"no", RestartPolicy{Name: "no"}, true},
		{"always", RestartPolicy{Name: "always"}, true},
		{"unless-stopped", RestartPolicy{Name: "unless-stopped"}, true},
		{"on-failure", RestartPolicy{Name: "on-failure"}, true},
		{"on-failure:3", RestartPolicy{Name: "on-failure", MaximumRetryCount: 3}, true},
		{"on-failure:-1", RestartPolicy{}, false},
		{"on-failure:x", RestartPolicy{}, false},
		{"always:2", RestartPolicy{}, false},
		{"sometimes", RestartPolicy{}, false},
		{"on-failure:99999999", RestartPolicy{}, false},
	} {
		got, err := ParseRestartPolicy(c.in)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("%q: %+v, %v", c.in, got, err)
		}
	}
	if err := checkRestart(Config{AutoRemove: true, Restart: RestartPolicy{Name: "always"}}); err == nil {
		t.Error("--rm with --restart always accepted")
	}
	if err := checkRestart(Config{AutoRemove: true, Restart: RestartPolicy{Name: "no"}}); err != nil {
		t.Error(err)
	}
}

// TestRestartOnFailure: on-failure:2 runs a failing container three times
// in all, with a growing delay, then leaves it exited; a run that succeeds
// is not restarted.
func TestRestartOnFailure(t *testing.T) {
	img := image(t)
	m := manager(t, Options{})
	ctx := context.Background()
	c, err := m.Create(CreateOptions{Name: "flaky", Lower: img, Config: Config{Image: "probe",
		Entrypoint: []string{"probe"}, Cmd: []string{"exit", "3"}, Restart: RestartPolicy{Name: "on-failure", MaximumRetryCount: 2}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := m.Start(ctx, "flaky"); err != nil {
		t.Fatal(err)
	}
	until(t, "two restarts and a final exit", 30*time.Second, func() bool {
		s := c.Snapshot()
		return s.RestartCount == 2 && s.State == Exited
	})
	time.Sleep(500 * time.Millisecond) // no third restart
	s := c.Snapshot()
	if s.State != Exited || s.RestartCount != 2 || s.ExitCode != 3 {
		t.Fatalf("%+v", s)
	}
	if _, stderr := output(t, m, "flaky"); strings.Count(stderr, "bye") != 3 {
		t.Fatalf("runs: %q", stderr)
	}
	c.mu.Lock()
	delay := c.restartDelay
	c.mu.Unlock()
	if delay != 2*RestartFirstDelay {
		t.Errorf("delay after two quick runs %v, want %v", delay, 2*RestartFirstDelay)
	}

	ok, err := m.Create(CreateOptions{Name: "fine", Lower: img, Config: Config{Image: "probe",
		Entrypoint: []string{"probe"}, Cmd: []string{"exit", "0"}, Restart: RestartPolicy{Name: "on-failure"}}})
	if err != nil {
		t.Fatal(err)
	}
	m.Start(ctx, "fine")
	m.Wait(ctx, "fine", WaitNotRunning)
	time.Sleep(300 * time.Millisecond)
	if s := ok.Snapshot(); s.State != Exited || s.RestartCount != 0 {
		t.Fatalf("a successful run restarted: %+v", s)
	}
}

// TestRestartAlwaysAndStop: always restarts a container that exits with
// 0; a user's stop ends it for good (also during the delay), and a user's
// start counts from zero again.
func TestRestartAlwaysAndStop(t *testing.T) {
	img := image(t)
	m := manager(t, Options{})
	ctx := context.Background()
	c, err := m.Create(CreateOptions{Name: "svc", Lower: img, Config: Config{Image: "probe",
		Entrypoint: []string{"probe"}, Cmd: []string{"exit", "0"}, Restart: RestartPolicy{Name: "always"}}})
	if err != nil {
		t.Fatal(err)
	}
	m.Start(ctx, "svc")
	until(t, "a restart", 30*time.Second, func() bool { return c.Snapshot().RestartCount >= 1 })
	// ps without -a lists a container waiting for its restart.
	until(t, "the restarting state", 30*time.Second, func() bool { return c.Snapshot().State == Restarting })
	if ls := m.List(false); len(ls) != 1 || ls[0].Name != "svc" {
		t.Fatalf("ps: %+v", ls)
	}
	if err := m.Remove(ctx, "svc", false); !errors.Is(err, ErrRunning) {
		t.Fatalf("removing a restarting container without force: %v", err)
	}
	if err := m.Stop(ctx, "svc"); err != nil {
		t.Fatal(err)
	}
	until(t, "stopped", 10*time.Second, func() bool { return c.Snapshot().State == Exited })
	n := c.Snapshot().RestartCount
	time.Sleep(600 * time.Millisecond)
	if s := c.Snapshot(); s.State != Exited || s.RestartCount != n {
		t.Fatalf("restarted after a stop: %+v (was %d)", s, n)
	}
	// A user's start: the policy acts again, from zero.
	m.Start(ctx, "svc")
	until(t, "a restart after the user's start", 30*time.Second, func() bool {
		s := c.Snapshot()
		return s.RestartCount >= 1 && s.RestartCount <= 3
	})
	if err := m.Kill("svc"); err != nil && !errors.Is(err, ErrNotRunning) {
		t.Fatal(err)
	}
	m.Wait(ctx, "svc", WaitNotRunning)
	until(t, "killed for good", 10*time.Second, func() bool { return c.Snapshot().State == Exited })
	if err := m.Remove(ctx, "svc", false); err != nil {
		t.Fatal(err)
	}
}

// TestAutoStart: with the engine, always starts even after a user's stop;
// unless-stopped only when no user stopped it; on-failure never.
func TestAutoStart(t *testing.T) {
	img := image(t)
	m := manager(t, Options{})
	ctx := context.Background()
	mk := func(name, policy string) *Container {
		c, err := m.Create(CreateOptions{Name: name, Lower: img, Config: Config{Image: "probe",
			Entrypoint: []string{"probe"}, Cmd: []string{"spin"}, Restart: RestartPolicy{Name: policy}}})
		if err != nil {
			t.Fatal(err)
		}
		return c
	}
	always, unless, stopped, onfail := mk("a", "always"), mk("u", "unless-stopped"), mk("s", "unless-stopped"), mk("f", "on-failure")
	stopped.stopByUser()
	if errs := m.AutoStart(ctx); len(errs) != 0 {
		t.Fatal(errs)
	}
	for _, c := range []*Container{always, unless} {
		if s := c.Snapshot(); s.State != Running {
			t.Errorf("%s: %s, want running", s.Name, s.State)
		}
	}
	for _, c := range []*Container{stopped, onfail} {
		if s := c.Snapshot(); s.State != Created {
			t.Errorf("%s: %s, want created", s.Name, s.State)
		}
	}
	m.Shutdown(ctx)
	if s := always.Snapshot(); s.State == Running || s.State == Restarting {
		t.Errorf("after shutdown: %s", s.State)
	}
}
