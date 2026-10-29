package cli

import (
	"strings"
	"testing"
	"time"
)

// TestRestartPolicies: run --restart through the API and inspect, the
// conflict with --rm, and the policies across an engine restart.
func TestRestartPolicies(t *testing.T) {
	layout := testLayout(t)
	dir := shortDir(t)
	h, stop := daemonAt(t, dir)
	must(t, wctr(t, h, "pull", "oci:"+layout+":probe"), 0)

	must(t, wctr(t, h, "run", "-d", "--name", "flaky", "--restart", "on-failure:1", "probe", "exit", "3"), 0)
	deadline := time.Now().Add(30 * time.Second)
	for {
		out := must(t, wctr(t, h, "inspect", "flaky"), 0).stdout
		if strings.Contains(out, `"RestartCount": 1`) && strings.Contains(out, `"Status": "exited"`) {
			if !strings.Contains(out, `"Name": "on-failure"`) || !strings.Contains(out, `"MaximumRetryCount": 1`) {
				t.Fatalf("inspect flaky:\n%s", out)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no restart:\n%s", out)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if r := wctr(t, h, "run", "--rm", "--restart", "always", "probe", "exit", "0"); r.code != 125 || !strings.Contains(r.stderr, "--restart and --rm") {
		t.Fatalf("--rm with --restart: %+v", r)
	}
	for _, bad := range []string{"sometimes", "always:3", "on-failure:x"} {
		if r := wctr(t, h, "run", "--restart", bad, "probe", "exit", "0"); r.code != 125 {
			t.Fatalf("--restart %s: %+v", bad, r)
		}
	}
	if out := must(t, wctr(t, h, "inspect", "flaky"), 0).stdout; !strings.Contains(out, `"Restarting": false`) {
		t.Fatalf("inspect lacks Restarting:\n%s", out)
	}

	// Across an engine restart: always comes back, unless-stopped too unless
	// a user stopped it, a container without a policy does not.
	must(t, wctr(t, h, "run", "-d", "--name", "svc", "--restart", "always", "probe", "spin"), 0)
	must(t, wctr(t, h, "run", "-d", "--name", "kept", "--restart", "unless-stopped", "probe", "spin"), 0)
	must(t, wctr(t, h, "run", "-d", "--name", "halted", "--restart", "unless-stopped", "probe", "spin"), 0)
	must(t, wctr(t, h, "run", "-d", "--name", "plain", "probe", "spin"), 0)
	must(t, wctr(t, h, "stop", "halted"), 0)
	stop()

	h, stop = daemonAt(t, dir)
	defer stop()
	ps := must(t, wctr(t, h, "ps"), 0).stdout
	for _, want := range []string{"svc", "kept"} {
		if !strings.Contains(ps, want) {
			t.Fatalf("ps lacks %s after the engine restart:\n%s", want, ps)
		}
	}
	for _, not := range []string{"halted", "plain"} {
		if strings.Contains(ps, not) {
			t.Fatalf("ps has %s after the engine restart:\n%s", not, ps)
		}
	}
	must(t, wctr(t, h, "rm", "-f", "svc", "kept", "halted", "plain", "flaky"), 0)
}
