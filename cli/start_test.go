package cli

import (
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/container"
)

func TestStartStopRestart(t *testing.T) {
	h := startDaemon(t, container.Options{})
	layout := testLayout(t)
	must(t, wctr(t, h, "pull", "oci:"+layout+":probe"), 0)

	// start -a: the run's own output, and its status.
	must(t, wctr(t, h, "run", "--name", "e", "probe", "exit", "7"), 7)
	r := must(t, wctr(t, h, "start", "-a", "e"), 7)
	if r.stderr != "bye\n" { // this run's only: attach is from now on
		t.Fatalf("%q", r.stderr)
	}
	if r := wctr(t, h, "start", "-a", "e", "e"); r.code != 1 || !strings.Contains(r.stderr, "cannot start and attach multiple") {
		t.Fatalf("%+v", r)
	}
	must(t, wctr(t, h, "run", "--name", "a", "probe", "args", "one", "two"), 0)
	if r := must(t, wctr(t, h, "start", "-a", "a"), 0); r.stdout != "one\ntwo\n" {
		t.Fatalf("%q", r.stdout)
	}

	// stop: running → exited, at once; stopping a stopped one is fine.
	must(t, wctr(t, h, "run", "-d", "--name", "s", "probe", "spin"), 0)
	if r := must(t, wctr(t, h, "stop", "s"), 0); r.stdout != "s\n" {
		t.Fatalf("%q", r.stdout)
	}
	if ps := must(t, wctr(t, h, "ps", "-q"), 0).stdout; ps != "" {
		t.Fatalf("still running: %q", ps)
	}
	must(t, wctr(t, h, "stop", "-t", "3", "s"), 0)

	// restart: a stopped one starts; a running one is stopped and started.
	must(t, wctr(t, h, "restart", "s"), 0)
	if ps := must(t, wctr(t, h, "ps"), 0).stdout; !strings.Contains(ps, " s\n") && !strings.HasSuffix(strings.TrimSpace(ps), " s") {
		t.Fatalf("not running after restart:\n%s", ps)
	}
	must(t, wctr(t, h, "restart", "s"), 0)
	must(t, wctr(t, h, "kill", "s"), 0)
	if r := wctr(t, h, "stop", "nosuch"); r.code == 0 {
		t.Fatal("stop of a missing container succeeded")
	}
}
