package cli

import (
	"strings"
	"testing"
	"time"
)

func TestVolumes(t *testing.T) {
	layout := testLayout(t)
	dir := shortDir(t)
	h, stop := daemonAt(t, dir)
	must(t, wctr(t, h, "pull", "oci:"+layout+":probe"), 0)

	if r := must(t, wctr(t, h, "volume", "create", "--label", "k=v", "data"), 0); r.stdout != "data\n" {
		t.Fatalf("%q", r.stdout)
	}
	// One container writes, another reads, a read-only one cannot write.
	must(t, wctr(t, h, "run", "--name", "w", "-v", "data:/data", "probe", "write", "/data/x", "hello"), 0)
	if r := must(t, wctr(t, h, "run", "--rm", "-v", "data:/data:ro", "probe", "cat", "/data/x"), 0); r.stdout != "hello" {
		t.Fatalf("%q", r.stdout)
	}
	if r := wctr(t, h, "run", "--rm", "-v", "data:/data:ro", "probe", "write", "/data/y", "no"); r.code != 1 || !strings.Contains(strings.ToLower(r.stderr), "read-only") {
		t.Fatalf("%+v", r)
	}
	// The container's own files are not the volume's.
	if r := must(t, wctr(t, h, "diff", "w"), 0); strings.Contains(r.stdout, "/data/x") {
		t.Fatalf("the volume's file is in the container's layer:\n%s", r.stdout)
	}
	// A volume named on first use is created.
	must(t, wctr(t, h, "run", "--rm", "-v", "fresh:/f", "probe", "ls", "/f"), 0)
	if r := must(t, wctr(t, h, "volume", "ls", "-q"), 0); r.stdout != "data\nfresh\n" {
		t.Fatalf("%q", r.stdout)
	}
	// Refusals: a host path, a bad mode, a relative target.
	for _, bad := range []string{"/tmp:/x", "./d:/x", "data:/x:rx", "data:x", "data:/"} {
		if r := wctr(t, h, "run", "--rm", "-v", bad, "probe", "args"); r.code != 125 {
			t.Errorf("-v %s: %+v", bad, r)
		}
	}
	if r := must(t, wctr(t, h, "inspect", "w"), 0); !strings.Contains(r.stdout, `"Destination": "/data"`) || !strings.Contains(r.stdout, `"Name": "data"`) {
		t.Fatalf("%s", r.stdout)
	}
	if r := must(t, wctr(t, h, "volume", "inspect", "data"), 0); !strings.Contains(r.stdout, `"k": "v"`) {
		t.Fatalf("%s", r.stdout)
	}
	if r := must(t, wctr(t, h, "system", "df"), 0); !strings.Contains(r.stdout, "Local Volumes   2") {
		t.Fatalf("%s", r.stdout)
	}

	// A restart: the volume, its content and the container's mount remain.
	stop()
	h, stop = daemonAt(t, dir)
	defer stop()
	if r := must(t, wctr(t, h, "run", "--rm", "-v", "data:/data", "probe", "cat", "/data/x"), 0); r.stdout != "hello" {
		t.Fatalf("after restart: %q", r.stdout)
	}
	must(t, wctr(t, h, "start", "-a", "w"), 0) // restored with its mount

	// In use: refused until the container is gone.
	if r := wctr(t, h, "volume", "rm", "data"); r.code == 0 || !strings.Contains(r.stderr, "in use") {
		t.Fatalf("%+v", r)
	}
	must(t, wctr(t, h, "rm", "w"), 0)
	must(t, wctr(t, h, "volume", "rm", "data"), 0)
	must(t, wctr(t, h, "volume", "rm", "-f", "data"), 0) // gone: fine with -f
	if r := wctr(t, h, "volume", "rm", "data"); r.code == 0 {
		t.Fatal("removing a missing volume succeeded")
	}
	// Prune: anonymous by default, all with -a.
	must(t, wctr(t, h, "volume", "create"), 0)
	if r := must(t, wctr(t, h, "volume", "prune", "-f"), 0); !strings.Contains(r.stdout, "Deleted Volumes:") {
		t.Fatalf("%q", r.stdout)
	}
	if r := must(t, wctr(t, h, "volume", "ls", "-q"), 0); r.stdout != "fresh\n" {
		t.Fatalf("%q", r.stdout)
	}
	must(t, wctr(t, h, "volume", "prune", "-a", "-f"), 0)
	if r := must(t, wctr(t, h, "volume", "ls", "-q"), 0); r.stdout != "" {
		t.Fatalf("%q", r.stdout)
	}
	// Volume events.
	until := time.Now().Add(time.Second).UTC().Format(time.RFC3339)
	if r := must(t, wctr(t, h, "events", "--since", "1m", "--until", until, "-f", "type=volume"), 0); !strings.Contains(r.stdout, "volume destroy data") || !strings.Contains(r.stdout, "volume prune") {
		t.Fatalf("%s", r.stdout)
	}
}
