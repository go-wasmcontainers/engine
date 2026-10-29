package server

import (
	"reflect"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/events"
)

// TestHealthAPI: the health check crosses the API in nanoseconds both
// ways, State.Health keeps the log, ps shows the status, and the event
// filter "health_status" matches "health_status: healthy".
func TestHealthAPI(t *testing.T) {
	in := &api.HealthConfig{Test: []string{"CMD", "probe"}, Interval: int64(2 * time.Second), Timeout: 7, StartPeriod: 8, StartInterval: 9, Retries: 4}
	if got := healthToAPI(healthFromAPI(in)); !reflect.DeepEqual(got, in) {
		t.Fatalf("round trip: %+v", got)
	}
	if healthFromAPI(nil) != nil || healthToAPI(nil) != nil || healthState(nil) != nil {
		t.Fatal("nil not kept")
	}
	t0 := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	st := healthState(&container.Health{Status: container.HealthUnhealthy, FailingStreak: 3,
		Log: []container.HealthResult{{Start: t0, End: t0.Add(time.Second), ExitCode: 1, Output: "no"}}})
	if st.Status != "unhealthy" || st.FailingStreak != 3 || len(st.Log) != 1 || st.Log[0].ExitCode != 1 || st.Log[0].Output != "no" || st.Log[0].Start == "" {
		t.Fatalf("state: %+v", st)
	}

	now := t0.Add(time.Minute)
	for status, want := range map[string]string{container.HealthStarting: " (health: starting)",
		container.HealthHealthy: " (healthy)", container.HealthUnhealthy: " (unhealthy)"} {
		s := container.Snapshot{State: container.Running, StartedAt: t0, Health: &container.Health{Status: status}}
		if got := humanStatus(s, now); got != "Up About a minute"+want {
			t.Errorf("%s: %q", status, got)
		}
	}

	f, err := eventFilter(`{"event":["health_status"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if !f(events.Event{Action: "health_status: healthy"}) || f(events.Event{Action: "health_statusx"}) || f(events.Event{Action: "die"}) {
		t.Fatal("health_status filter")
	}
}
