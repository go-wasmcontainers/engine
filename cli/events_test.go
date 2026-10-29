package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/api/server"
	"github.com/go-wasmcontainers/engine/container"
)

func TestEvents(t *testing.T) {
	h := startDaemon(t, container.Options{})
	layout := testLayout(t)
	t0 := time.Now().Add(-time.Second).Unix()

	// A live subscriber, started before anything happens.
	c, err := api.NewClient(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live := make(chan api.EventMessage, 64)
	go c.Events(ctx, api.EventsOptions{Filters: map[string][]string{"type": {"container"}}}, func(m api.EventMessage) error {
		live <- m
		return nil
	})
	time.Sleep(100 * time.Millisecond)

	must(t, wctr(t, h, "pull", "oci:"+layout+":probe"), 0)
	// Names with a '-': `-f container=NAME` also matches an ID prefix (as
	// Docker's), and a one-letter hex name such as "e" matched a random ID
	// starting with it now and then (1 run in 16).
	must(t, wctr(t, h, "run", "--name", "ev-e", "probe", "exit", "3"), 3)
	must(t, wctr(t, h, "run", "-d", "--name", "ev-s", "probe", "spin"), 0)
	must(t, wctr(t, h, "stop", "ev-s"), 0)
	must(t, wctr(t, h, "rm", "ev-e", "ev-s"), 0)

	var got []string
	timeout := time.After(10 * time.Second)
	for len(got) < 10 {
		select {
		case m := <-live:
			got = append(got, m.Action+":"+m.Actor.Attributes["name"])
			if m.Action == "die" && m.Actor.Attributes["name"] == "ev-e" && m.Actor.Attributes["exitCode"] != "3" {
				t.Fatalf("die without its exit code: %+v", m)
			}
		case <-timeout:
			t.Fatalf("live events: %v", got)
		}
	}
	want := "create:ev-e,start:ev-e,die:ev-e,create:ev-s,start:ev-s,kill:ev-s,die:ev-s,stop:ev-s,destroy:ev-e,destroy:ev-s"
	if strings.Join(got, ",") != want {
		t.Fatalf("live events\n got %s\nwant %s", strings.Join(got, ","), want)
	}

	// The same from history, with --since/--until, in docker's format.
	until := time.Now().Add(time.Second).Unix()
	r := must(t, wctr(t, h, "events", "--since", itoa(t0), "--until", itoa(until), "-f", "container=ev-e"), 0)
	lines := strings.Split(strings.TrimSpace(r.stdout), "\n")
	if len(lines) != 4 || !strings.Contains(lines[2], " container die ") || !strings.Contains(lines[2], "exitCode=3") || !strings.Contains(lines[2], "image=probe, name=ev-e") {
		t.Fatalf("%q", r.stdout)
	}
	r = must(t, wctr(t, h, "events", "--since", "1m", "--until", itoa(until), "-f", "type=image"), 0)
	if !strings.Contains(r.stdout, " image pull probe") {
		t.Fatalf("%q", r.stdout)
	}
	// Refusals: an unknown filter, a bad time.
	if r := wctr(t, h, "events", "-f", "nope=1", "--until", itoa(until)); r.code == 0 {
		t.Fatal("unknown filter accepted")
	}
	if r := wctr(t, h, "events", "--since", "yesterday"); r.code == 0 {
		t.Fatal("bad time accepted")
	}
}

// itoa renders Unix seconds as RFC 3339, one of the forms --since takes.
func itoa(n int64) string { return time.Unix(n, 0).UTC().Format(time.RFC3339) }

func TestParseEventTime(t *testing.T) {
	now := time.Unix(1000, 0)
	for in, want := range map[string]time.Time{
		"":                     {},
		"1629574695":           time.Unix(1629574695, 0),
		"1629574695.5":         time.Unix(1629574695, 5e8),
		"10m":                  now.Add(-10 * time.Minute),
		"2021-08-21T19:38:15Z": time.Unix(1629574695, 0),
	} {
		got, err := server.ParseEventTime(in, now)
		if err != nil || !got.Equal(want) {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"x", "-5", "1.x", "1.1234567890", "99999999999999999999"} {
		if _, err := server.ParseEventTime(bad, now); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
