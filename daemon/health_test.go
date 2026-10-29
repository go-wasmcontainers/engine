package daemon

import (
	"reflect"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/image"
)

// TestMergeHealth: the request's check is completed field by field by the
// image's HEALTHCHECK, as Docker merges them; no test anywhere is no check.
func TestMergeHealth(t *testing.T) {
	img := &image.HealthConfig{Test: []string{"CMD", "probe"}, Interval: int64(time.Second), Timeout: int64(2 * time.Second), Retries: 5}
	if mergeHealth(nil, nil) != nil || mergeHealth(&container.HealthConfig{Retries: 2}, nil) != nil {
		t.Fatal("a check without a test")
	}
	got := mergeHealth(nil, img)
	want := &container.HealthConfig{Test: []string{"CMD", "probe"}, Interval: time.Second, Timeout: 2 * time.Second, Retries: 5}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("image only: %+v", got)
	}
	got = mergeHealth(&container.HealthConfig{Retries: 1, Interval: time.Minute}, img)
	want = &container.HealthConfig{Test: []string{"CMD", "probe"}, Interval: time.Minute, Timeout: 2 * time.Second, Retries: 1}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("tuned: %+v", got)
	}
	got = mergeHealth(&container.HealthConfig{Test: []string{"NONE"}}, img)
	if got == nil || got.Test[0] != "NONE" || got.Enabled() {
		t.Fatalf("--no-healthcheck: %+v", got)
	}
}
