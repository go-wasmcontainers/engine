package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/spf13/cobra"
)

// TestHealthFlags: run's --health-* flags become the create request's
// Healthcheck; none given leaves the image's check; --no-healthcheck
// disables it and refuses to be combined with the others.
func TestHealthFlags(t *testing.T) {
	parse := func(args ...string) (*api.HealthConfig, error) {
		var h healthFlags
		cmd := &cobra.Command{Use: "run"}
		h.register(cmd.Flags())
		if err := cmd.Flags().Parse(args); err != nil {
			t.Fatal(err)
		}
		return h.config(cmd)
	}
	if got, err := parse(); got != nil || err != nil {
		t.Fatalf("no flag: %+v %v", got, err)
	}
	got, err := parse("--health-cmd", "test -f /ready", "--health-interval", "2s", "--health-timeout", "1s",
		"--health-start-period", "10s", "--health-start-interval", "500ms", "--health-retries", "4")
	want := &api.HealthConfig{Test: []string{"CMD-SHELL", "test -f /ready"}, Interval: int64(2 * time.Second),
		Timeout: int64(time.Second), StartPeriod: int64(10 * time.Second), StartInterval: int64(500 * time.Millisecond), Retries: 4}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("flags: %+v %v", got, err)
	}
	// Tuning without a command keeps the image's test.
	if got, err := parse("--health-retries", "1"); err != nil || got == nil || got.Test != nil || got.Retries != 1 {
		t.Fatalf("retries only: %+v %v", got, err)
	}
	if got, err := parse("--no-healthcheck"); err != nil || !reflect.DeepEqual(got.Test, []string{"NONE"}) {
		t.Fatalf("--no-healthcheck: %+v %v", got, err)
	}
	if _, err := parse("--no-healthcheck", "--health-cmd", "true"); err == nil || !strings.Contains(err.Error(), "conflicts") {
		t.Fatalf("conflict: %v", err)
	}
}
