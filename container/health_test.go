package container

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/events"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
)

// engineManager is a manager whose executor runs the engine's WASI host,
// which exec, and so health probes, need.
func engineManager(t *testing.T, bus *events.Bus) *Manager {
	e := wazero.New(wazero.Options{EngineWASI: true})
	t.Cleanup(func() { e.Close(context.Background()) })
	return NewManager(Options{Executor: e, Events: bus})
}

func TestBoundedBuffer(t *testing.T) {
	b := &boundedBuffer{n: 5}
	for _, s := range []string{"abc", "defgh", "ij"} {
		if n, err := b.Write([]byte(s)); n != len(s) || err != nil {
			t.Fatalf("write %q: %d %v", s, n, err)
		}
	}
	if b.String() != "abcde" {
		t.Fatalf("kept %q", b.String())
	}
}

func TestCheckHealth(t *testing.T) {
	for _, bad := range []HealthConfig{
		{Test: []string{"SOMETIMES"}},
		{Test: []string{"CMD"}},
		{Test: []string{"CMD-SHELL"}},
		{Test: []string{"CMD-SHELL", "a", "b"}},
		{Test: []string{"NONE", "x"}},
		{Test: []string{"CMD", "probe"}, Interval: time.Millisecond},
		{Test: []string{"CMD", "probe"}, Timeout: time.Millisecond},
		{Test: []string{"CMD", "probe"}, StartPeriod: -time.Second},
		{Test: []string{"CMD", "probe"}, Retries: -1},
		{Test: []string{"CMD", strings.Repeat("x", maxHealthTest+1)}},
	} {
		if err := checkHealth(&bad); err == nil {
			t.Errorf("accepted %+v", bad.Test)
		}
	}
	for _, good := range []HealthConfig{
		{Test: []string{"NONE"}},
		{Test: []string{"CMD", "probe", "exit", "0"}, Interval: time.Second, Retries: 2},
		{Test: []string{"CMD-SHELL", "curl -f localhost"}},
	} {
		if err := checkHealth(&good); err != nil {
			t.Errorf("%+v: %v", good.Test, err)
		}
	}
	if (&HealthConfig{Test: []string{"NONE"}}).Enabled() || (*HealthConfig)(nil).Enabled() {
		t.Error("NONE or nil enabled")
	}
}

func healthOf(c *Container) Health {
	if h := c.Snapshot().Health; h != nil {
		return *h
	}
	return Health{}
}

// TestHealth: a passing probe makes the container healthy; a failing one
// unhealthy after Retries, with the output kept in a bounded log; a probe
// that outlives its timeout fails; the probes stop with the container.
func TestHealth(t *testing.T) {
	img := image(t)
	bus := events.New()
	m := engineManager(t, bus)
	ctx := context.Background()
	run := func(name string, h HealthConfig) *Container {
		c, err := m.Create(CreateOptions{Name: name, Lower: img, Config: Config{Image: "probe",
			Entrypoint: []string{"probe"}, Cmd: []string{"spin"}, Healthcheck: &h}})
		if err != nil {
			t.Fatal(err)
		}
		if err := m.Start(ctx, name); err != nil {
			t.Fatal(err)
		}
		return c
	}
	fast := 100 * time.Millisecond

	ok := run("ok", HealthConfig{Test: []string{"CMD", "probe", "exit", "0"}, Interval: fast})
	if h := healthOf(ok); h.Status != HealthStarting {
		t.Fatalf("before a probe: %+v", h)
	}
	until(t, "healthy", 30*time.Second, func() bool { return healthOf(ok).Status == HealthHealthy })

	bad := run("bad", HealthConfig{Test: []string{"CMD", "probe", "exit", "3"}, Interval: fast, Retries: 2})
	until(t, "unhealthy", 30*time.Second, func() bool { return healthOf(bad).Status == HealthUnhealthy })
	until(t, "a full log", 30*time.Second, func() bool { return len(healthOf(bad).Log) == HealthLogSize })
	time.Sleep(3 * fast)
	h := healthOf(bad)
	if len(h.Log) != HealthLogSize || h.FailingStreak < 2 {
		t.Fatalf("unhealthy: %+v", h)
	}
	if r := h.Log[len(h.Log)-1]; r.ExitCode != 3 || !strings.Contains(r.Output, "bye") || r.End.Before(r.Start) {
		t.Fatalf("last probe: %+v", r)
	}

	slow := run("slow", HealthConfig{Test: []string{"CMD", "probe", "spin"}, Interval: fast, Timeout: 200 * time.Millisecond, Retries: 1})
	until(t, "a timed-out probe", 30*time.Second, func() bool { return healthOf(slow).Status == HealthUnhealthy })
	if r := healthOf(slow).Log[0]; r.ExitCode != -1 || !strings.Contains(r.Output, "exceeded timeout") {
		t.Fatalf("timeout: %+v", r)
	}

	// In the start period a failure does not count.
	starting := run("starting", HealthConfig{Test: []string{"CMD", "probe", "exit", "1"}, Interval: fast,
		StartPeriod: time.Hour, StartInterval: fast, Retries: 1})
	until(t, "probes in the start period", 30*time.Second, func() bool { return len(healthOf(starting).Log) >= 2 })
	if h := healthOf(starting); h.Status != HealthStarting || h.FailingStreak != 0 {
		t.Fatalf("start period: %+v", h)
	}

	// The probes stop with the container.
	for _, n := range []string{"ok", "bad", "slow", "starting"} {
		m.Kill(n)
		m.Wait(ctx, n, WaitNotRunning)
	}
	n := len(healthOf(ok).Log)
	time.Sleep(5 * fast)
	if len(healthOf(ok).Log) != n {
		t.Fatal("probes after the container stopped")
	}

	// A change of status is an event; a probe is not an exec event.
	past, sub := bus.Subscribe(time.Unix(1, 0), nil)
	sub.Close()
	got := map[string]int{}
	for _, e := range past {
		got[e.Attributes["name"]+" "+e.Action]++
		if strings.HasPrefix(e.Action, "exec_") {
			t.Errorf("probe event %s %s", e.Attributes["name"], e.Action)
		}
	}
	for _, want := range []string{"ok health_status: healthy", "bad health_status: unhealthy", "slow health_status: unhealthy"} {
		if got[want] != 1 {
			t.Errorf("%q: %d events", want, got[want])
		}
	}
	if got["starting health_status: unhealthy"]+got["starting health_status: healthy"] != 0 {
		t.Error("status event in the start period")
	}
}
