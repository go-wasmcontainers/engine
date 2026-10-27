package cli

import (
	"encoding/json"
	"strings"
	"testing"
)

type inspected struct {
	ID     string `json:"Id"`
	Config struct {
		Env        []string
		Cmd        []string
		WorkingDir string
		User       string
	}
	RootFS struct {
		Layers []string
	}
	History []struct {
		CreatedBy  string `json:"created_by"`
		Comment    string `json:"comment"`
		EmptyLayer bool   `json:"empty_layer"`
	}
	RepoTags []string
}

func inspectImage(t *testing.T, h, ref string) inspected {
	t.Helper()
	r := must(t, wctr(t, h, "inspect", ref), 0)
	var out []inspected
	if err := json.Unmarshal([]byte(r.stdout), &out); err != nil || len(out) != 1 {
		t.Fatalf("inspect %s: %v\n%s", ref, err, r.stdout)
	}
	return out[0]
}

func TestCommitAndTag(t *testing.T) {
	layout := testLayout(t)
	dir := shortDir(t)
	h, stop := daemonAt(t, dir)
	must(t, wctr(t, h, "pull", "oci:"+layout+":probe"), 0)
	parent := inspectImage(t, h, "probe")

	// A container's file, committed, is in the image.
	must(t, wctr(t, h, "run", "--name", "w", "probe", "write", "/data.txt", "hi"), 0)
	id := strings.TrimSpace(must(t, wctr(t, h, "commit", "-m", "first", "w", "snap:v1"), 0).stdout)
	if !strings.HasPrefix(id, "sha256:") {
		t.Fatalf("commit printed %q", id)
	}
	if r := must(t, wctr(t, h, "run", "--rm", "snap:v1", "cat", "/data.txt"), 0); r.stdout != "hi" {
		t.Fatalf("committed file: %q", r.stdout)
	}
	snap := inspectImage(t, h, "snap:v1")
	if snap.ID != id || len(snap.RootFS.Layers) != len(parent.RootFS.Layers)+1 {
		t.Fatalf("layers %v, parent %v", snap.RootFS.Layers, parent.RootFS.Layers)
	}
	for _, l := range snap.RootFS.Layers {
		if !strings.HasPrefix(l, "sha256:") {
			t.Fatalf("layer %q", l)
		}
	}
	last := snap.History[len(snap.History)-1]
	if last.Comment != "first" || last.EmptyLayer || !strings.Contains(last.CreatedBy, "write /data.txt hi") {
		t.Fatalf("history %+v", snap.History)
	}

	// --change sets the config; the new CMD runs.
	must(t, wctr(t, h, "commit", "-c", "ENV A=1", "-c", `CMD ["cat", "/data.txt"]`, "-c", "WORKDIR /", "w", "withcmd"), 0)
	if r := must(t, wctr(t, h, "run", "--rm", "withcmd"), 0); r.stdout != "hi" {
		t.Fatalf("CMD from --change: %q", r.stdout)
	}
	wc := inspectImage(t, h, "withcmd")
	if !contains(wc.Config.Env, "A=1") || strings.Join(wc.Config.Cmd, " ") != "cat /data.txt" {
		t.Fatalf("config %+v", wc.Config)
	}
	if r := wctr(t, h, "commit", "-c", "RUN make", "w", "bad"); r.code == 0 {
		t.Fatal("RUN accepted as a change")
	}

	// No repository: a <none> image, addressable by ID.
	noname := strings.TrimSpace(must(t, wctr(t, h, "commit", "w"), 0).stdout)
	if r := must(t, wctr(t, h, "run", "--rm", noname, "cat", "/data.txt"), 0); r.stdout != "hi" {
		t.Fatalf("by ID: %q", r.stdout)
	}
	if r := must(t, wctr(t, h, "images"), 0); !strings.Contains(r.stdout, "<none>") {
		t.Fatalf("no <none> image:\n%s", r.stdout)
	}

	// An unchanged container: an empty layer, recorded as such.
	must(t, wctr(t, h, "run", "--name", "e", "probe", "exit", "0"), 0)
	must(t, wctr(t, h, "commit", "e", "empty"), 0)
	em := inspectImage(t, h, "empty")
	if len(em.RootFS.Layers) != len(parent.RootFS.Layers) || !em.History[len(em.History)-1].EmptyLayer {
		t.Fatalf("empty commit: %v %+v", em.RootFS.Layers, em.History)
	}

	// tag; moving a tag leaves the old image as <none>, still runnable.
	must(t, wctr(t, h, "tag", "snap:v1", "snap:v2"), 0)
	if inspectImage(t, h, "snap:v2").ID != id {
		t.Fatal("tag points elsewhere")
	}
	must(t, wctr(t, h, "tag", "empty", "snap:v1"), 0)
	if r := must(t, wctr(t, h, "run", "--rm", id, "cat", "/data.txt"), 0); r.stdout != "hi" {
		t.Fatalf("old image by ID: %q", r.stdout)
	}
	if r := wctr(t, h, "tag", "snap:v2", "x@sha256:"+strings.Repeat("a", 64)); r.code == 0 {
		t.Fatal("tagged with a digest")
	}

	// A CNCF wasm artifact (one application/wasm layer, no tar) commits
	// to an OCI image that still runs.
	must(t, wctr(t, h, "pull", "oci:"+layout+":greet"), 0)
	must(t, wctr(t, h, "run", "--name", "g", "-e", "NAME=commit", "greet"), 0)
	must(t, wctr(t, h, "commit", "g", "greet2"), 0)
	if r := must(t, wctr(t, h, "run", "--rm", "-e", "NAME=again", "greet2"), 0); r.stdout != "Hello, again!\n" {
		t.Fatalf("committed artifact: %q", r.stdout)
	}

	// A restart keeps the <none> images.
	stop()
	h, _ = daemonAt(t, dir)
	if r := must(t, wctr(t, h, "run", "--rm", noname, "cat", "/data.txt"), 0); r.stdout != "hi" {
		t.Fatalf("after restart: %q", r.stdout)
	}
	// rmi by ID removes a <none> image.
	if r := must(t, wctr(t, h, "rmi", noname), 0); !strings.Contains(r.stdout, "Deleted") {
		t.Fatalf("rmi: %s", r.stdout)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
