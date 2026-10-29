package cli

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api/server"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/daemon"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
	"github.com/go-wasmcontainers/engine/state"
)

// daemonAt serves an engine whose images and containers live under dir;
// stop shuts it down as wctrd does on SIGTERM.
func daemonAt(t *testing.T, dir string) (host string, stop func()) {
	t.Helper()
	e := wazero.New(wazero.Options{})
	st, err := state.OpenDir(filepath.Join(dir, "containers"))
	if err != nil {
		t.Fatal(err)
	}
	d, err := daemon.New(daemon.Options{Root: filepath.Join(dir, "data"), Containers: container.Options{Executor: e, Store: st}})
	if err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: server.New(d, server.Info{Version: "test"})}
	go srv.Serve(l)
	return "unix://" + sock, func() {
		d.Close(context.Background())
		srv.Close()
		e.Close(context.Background())
	}
}

func TestRestartKeepsContainers(t *testing.T) {
	layout := testLayout(t)
	dir := shortDir(t)
	h, stop := daemonAt(t, dir)
	must(t, wctr(t, h, "pull", "oci:"+layout+":probe"), 0)
	must(t, wctr(t, h, "run", "--name", "wrote", "probe", "fs"), 0)
	must(t, wctr(t, h, "run", "--name", "failed", "probe", "exit", "7"), 7)
	must(t, wctr(t, h, "run", "-d", "--name", "spinning", "probe", "spin"), 0)
	must(t, wctr(t, h, "run", "-d", "--rm", "--name", "rm", "probe", "spin"), 0)
	stop() // kills the running ones, as a restart of wctrd does

	h, stop = daemonAt(t, dir)
	defer stop()
	ps := must(t, wctr(t, h, "ps", "-a"), 0).stdout
	for _, want := range []string{"wrote", "Exited (0)", "failed", "Exited (7)", "spinning"} {
		if !strings.Contains(ps, want) {
			t.Fatalf("ps lacks %q:\n%s", want, ps)
		}
	}
	if strings.Contains(ps, " rm\n") || strings.HasSuffix(strings.TrimSpace(ps), " rm") {
		t.Fatalf("--rm container kept:\n%s", ps)
	}
	if r := must(t, wctr(t, h, "logs", "wrote"), 0); !strings.Contains(r.stdout, "motd: changed") {
		t.Fatal(r.stdout)
	}
	if r := must(t, wctr(t, h, "logs", "failed"), 0); r.stderr != "bye\n" {
		t.Fatalf("%q", r.stderr)
	}
	// Its writable layer came back.
	if r := must(t, wctr(t, h, "diff", "wrote"), 0); r.stdout != "C /etc\nC /etc/motd\nA /tmp\nA /tmp/probe\n" {
		t.Fatalf("%q", r.stdout)
	}
	// A restored container starts again.
	must(t, wctr(t, h, "start", "failed"), 0) // returns once started, as docker start does
	// Its second output, then its exit: the output arrives before the
	// process has ended, and a container still running when `system
	// prune` runs below is left alone (seen once on macOS CI: "left: <id>").
	for i := 0; ; i++ {
		r := must(t, wctr(t, h, "logs", "failed"), 0)
		in := must(t, wctr(t, h, "inspect", "failed"), 0).stdout
		if r.stderr == "bye\nbye\n" && strings.Contains(in, `"Status": "exited"`) {
			break
		}
		if i > 300 {
			t.Fatalf("%q\n%s", r.stderr, in)
		}
		time.Sleep(20 * time.Millisecond)
	}

	df := must(t, wctr(t, h, "system", "df"), 0).stdout
	for _, want := range []string{"TYPE", "Images", "Containers", "3", "State store: directory "} {
		if !strings.Contains(df, want) {
			t.Fatalf("df lacks %q:\n%s", want, df)
		}
	}
	if v := must(t, wctr(t, h, "system", "df", "-v"), 0).stdout; !strings.Contains(v, "Containers space usage:") || !strings.Contains(v, "wrote") {
		t.Fatal(v)
	}
	// Without -f the question gets no answer here, which is a no.
	if r := wctr(t, h, "system", "prune"); r.code != 1 || !strings.Contains(r.stdout, "Are you sure") {
		t.Fatalf("%+v", r)
	}
	r := must(t, wctr(t, h, "system", "prune", "-f"), 0)
	if !strings.Contains(r.stdout, "Deleted Containers:") || !strings.Contains(r.stdout, "Total reclaimed space:") {
		t.Fatal(r.stdout)
	}
	if ps := must(t, wctr(t, h, "ps", "-a", "-q"), 0).stdout; ps != "" {
		t.Fatalf("left: %q", ps)
	}
	// -a takes the unused images too.
	if r := must(t, wctr(t, h, "system", "prune", "-a", "-f"), 0); !strings.Contains(r.stdout, "Deleted Images:") || !strings.Contains(r.stdout, "untagged: probe:latest") {
		t.Fatal(r.stdout)
	}
	if imgs := must(t, wctr(t, h, "images", "-q"), 0).stdout; imgs != "" {
		t.Fatalf("left: %q", imgs)
	}
}
