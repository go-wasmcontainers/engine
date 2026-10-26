package container

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/state"
)

func persistImage(t *testing.T) *rootfs.FS {
	f := rootfs.New()
	f.MkdirAll("/usr/bin", 0o755)
	f.MkdirAll("/etc", 0o755)
	f.WriteFile("/usr/bin/probe.wasm", testutil.Wasm(t, "probe"), 0o755)
	f.WriteFile("/etc/motd", []byte("image\n"), 0o644)
	f.Freeze()
	return f
}

var restoreLimits = rootfs.Limits{MaxBytes: 64 << 20, MaxFiles: 1 << 12, MaxRatio: 1, MaxPathLen: 4096, MaxDepth: 128}

func probe(args ...string) Config {
	return Config{Image: "probe", ImageID: "sha256:img", Entrypoint: []string{"probe"}, Cmd: args}
}

func lowerOf(img *rootfs.FS) func(string) (*rootfs.FS, error) {
	return func(id string) (*rootfs.FS, error) {
		if id != "sha256:img" {
			return nil, errors.New("no such image")
		}
		return img, nil
	}
}

func waitStored(t *testing.T, s state.Store, id string, st State) {
	t.Helper()
	for i := 0; i < 500; i++ {
		if v, err := s.Get("container/" + id); err == nil {
			if r, err := DecodeRecord(v); err == nil && r.State == st {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never stored as %s", id[:12], st)
}

func TestPersistAndRestore(t *testing.T) {
	img := persistImage(t)
	store := &state.Mem{}
	m1 := manager(t, Options{Store: store, CheckpointEvery: 20 * time.Millisecond})
	t.Cleanup(func() { m1.Shutdown(context.Background()) })
	ctx := context.Background()

	wrote, code := runToExit(t, m1, CreateOptions{Name: "wrote", Lower: img, Config: probe("fs")})
	if code != 0 {
		o, e := output(t, m1, wrote.ID())
		t.Fatal(code, o, e)
	}
	failed, _ := runToExit(t, m1, CreateOptions{Name: "failed", Lower: img, Config: probe("exit", "7")})
	created, err := m1.Create(CreateOptions{Name: "created", Lower: img, Config: probe("args", "x")})
	if err != nil {
		t.Fatal(err)
	}
	var spinning []*Container
	for _, o := range []CreateOptions{
		{Name: "spinning", Lower: img, Config: probe("spin")},
		{Name: "rm", Lower: img, Config: func() Config { c := probe("spin"); c.AutoRemove = true; return c }()},
	} {
		c, err := m1.Create(o)
		if err != nil {
			t.Fatal(err)
		}
		if err := m1.Start(ctx, c.ID()); err != nil {
			t.Fatal(err)
		}
		waitStored(t, store, c.ID(), Running)
		spinning = append(spinning, c)
	}

	// A second engine on the same store, as after a reload: m1 never shut
	// down, as when a tab is closed (its writer has caught up).
	waitStored(t, store, wrote.ID(), Exited)
	waitStored(t, store, failed.ID(), Exited)
	if err := m1.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	m2 := manager(t, Options{Store: store})
	rep, err := m2.Restore(lowerOf(img), restoreLimits)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Restored != 5 || rep.Stopped != 2 || len(rep.Removed) != 1 || rep.Removed[0] != spinning[1].ID() || len(rep.Repaired) != 0 {
		t.Fatalf("%+v", rep)
	}
	byName := map[string]Snapshot{}
	for _, s := range m2.List(true) {
		byName[s.Name] = s
	}
	if len(byName) != 4 {
		t.Fatalf("%v", byName)
	}
	if s := byName["spinning"]; s.State != Exited || s.ExitCode != 137 || s.Error == "" {
		t.Fatalf("%+v", s)
	}
	if s := byName["failed"]; s.State != Exited || s.ExitCode != 7 {
		t.Fatalf("%+v", s)
	}
	if _, e := output(t, m2, failed.ID()); e != "bye\n" {
		t.Fatalf("logs %q", e)
	}
	if o, _ := output(t, m2, wrote.ID()); !strings.Contains(o, "appended: alpha+beta") {
		t.Fatalf("logs %q", o)
	}
	if s := byName["created"]; s.State != Created || s.ID != created.ID() {
		t.Fatalf("%+v", s)
	}
	// The writable layer came back: the container's own /etc/motd, its
	// /tmp/probe, and the same usage.
	w, _ := m2.Get("wrote")
	if b, err := w.FS().ReadFile("/etc/motd"); err != nil || string(b) != "changed\n" {
		t.Fatalf("%q %v", b, err)
	}
	if _, err := w.FS().Stat("/tmp/probe"); err != nil {
		t.Fatal(err)
	}
	if byName["wrote"].SizeRw != wrote.Snapshot().SizeRw || byName["wrote"].SizeRw == 0 {
		t.Fatalf("SizeRw %d, was %d", byName["wrote"].SizeRw, wrote.Snapshot().SizeRw)
	}
	if b, _ := img.ReadFile("/etc/motd"); string(b) != "image\n" {
		t.Fatal("the image changed")
	}
	// Restored containers run again, and are stored again.
	if err := m2.Start(ctx, "wrote"); err != nil {
		t.Fatal(err)
	}
	if code, _ := m2.Wait(ctx, "wrote", WaitNotRunning); code != 0 {
		t.Fatal(code)
	}
	// Removing one deletes its record and the blobs only it named.
	if err := m2.Remove(ctx, "failed", false); err != nil {
		t.Fatal(err)
	}
	m2.Flush(ctx)
	if _, err := store.Get("container/" + failed.ID()); !errors.Is(err, state.ErrNotFound) {
		t.Fatal("record kept")
	}
	// What is stored is exactly what m2 has: a third engine sees the same.
	m3 := manager(t, Options{Store: store})
	rep, _ = m3.Restore(lowerOf(img), restoreLimits)
	if rep.Restored != 3 || rep.Stopped != 0 || len(rep.Repaired) != 0 {
		t.Fatalf("%+v", rep)
	}
	referenced := map[string]bool{}
	for _, c := range m3.byID {
		referenced[c.persisted.logsKey], referenced[c.persisted.diffKey] = true, true
	}
	blobs, _ := store.Keys("blob/")
	for _, b := range blobs {
		if !referenced[b] {
			t.Errorf("unreferenced blob %s kept", b)
		}
	}
	// Prune removes what is not running, from the store too.
	ids, _ := m3.Prune()
	if len(ids) != 3 {
		t.Fatal(ids)
	}
	m3.Flush(ctx)
	if ks, _ := store.Keys(""); len(ks) != 0 {
		t.Fatalf("left in the store: %v", ks)
	}
}

func TestRestoreRepairs(t *testing.T) {
	img := persistImage(t)
	store := &state.Mem{}
	m1 := manager(t, Options{Store: store})
	var ids []string
	for _, n := range []string{"badrecord", "badlogs", "baddiff", "noimage", "moved", "fine"} {
		c, _ := runToExit(t, m1, CreateOptions{Name: n, Lower: img, Config: probe("fs")})
		ids = append(ids, c.ID())
	}
	m1.exits.Wait()
	m1.Flush(context.Background())
	get := func(id string) *record {
		v, _ := store.Get("container/" + id)
		r, err := DecodeRecord(v)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	put := func(key string, r *record) {
		raw, _ := json.Marshal(r)
		store.Put(key, state.Seal(recordKind, raw))
	}
	// A flipped byte in a record.
	v, _ := store.Get("container/" + ids[0])
	v[len(v)-3] ^= 1
	store.Put("container/"+ids[0], v)
	// Logs and layer blobs that no longer match their keys: give each
	// container its own copy first (identical runs share blobs).
	r := get(ids[1])
	r.Logs = "blob/" + strings.Repeat("a", 64)
	store.Put(r.Logs, []byte("not the logs"))
	put("container/"+ids[1], r)
	r = get(ids[2])
	r.Diff = "blob/" + strings.Repeat("b", 64)
	store.Put(r.Diff, []byte("not a layer"))
	put("container/"+ids[2], r)
	// An image that is gone.
	r = get(ids[3])
	r.ImageID = "sha256:gone"
	put("container/"+ids[3], r)
	// A record under another container's key.
	r = get(ids[4])
	store.Delete("container/" + ids[4])
	put("container/"+strings.Repeat("c", 64), r)
	// A blob no record names (a crash between blob and record).
	store.Put("blob/"+strings.Repeat("d", 64), []byte("orphan"))

	m2 := manager(t, Options{Store: store})
	rep, err := m2.Restore(lowerOf(img), restoreLimits)
	if err != nil {
		t.Fatal(err)
	}
	if rep.Restored != 3 || len(rep.Repaired) != 5 {
		t.Fatalf("%d restored, repairs:\n%s", rep.Restored, strings.Join(rep.Repaired, "\n"))
	}
	if c, err := m2.Get("badlogs"); err != nil {
		t.Fatal(err)
	} else if o, _ := output(t, m2, c.ID()); o != "" {
		t.Fatal("corrupt logs kept")
	}
	if c, err := m2.Get("baddiff"); err != nil {
		t.Fatal(err)
	} else if b, _ := c.FS().ReadFile("/etc/motd"); string(b) != "image\n" {
		t.Fatalf("corrupt layer applied: %q", b)
	}
	if c, _ := m2.Get("fine"); c == nil {
		t.Fatal("fine lost")
	} else if b, _ := c.FS().ReadFile("/etc/motd"); string(b) != "changed\n" {
		t.Fatal("fine lost its layer")
	}
	for _, k := range []string{"blob/" + strings.Repeat("d", 64), "container/" + ids[0], "container/" + strings.Repeat("c", 64)} {
		if _, err := store.Get(k); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("%s kept", k)
		}
	}
	// The repairs were stored: a third load finds nothing to repair.
	m3 := manager(t, Options{Store: store})
	if rep, _ := m3.Restore(lowerOf(img), restoreLimits); rep.Restored != 3 || len(rep.Repaired) != 0 {
		t.Fatalf("%+v", rep)
	}
}

func TestPersistFullStore(t *testing.T) {
	img := persistImage(t)
	store := &state.Mem{MaxBytes: 600}
	m := manager(t, Options{Store: store})
	c, code := runToExit(t, m, CreateOptions{Name: "big", Lower: img, Config: probe("fs")})
	if code != 0 {
		t.Fatal(code)
	}
	// The exit is stored just after waiters are woken.
	for i := 0; i < 200 && m.PersistError() == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if err := m.PersistError(); !errors.Is(err, state.ErrNoSpace) {
		t.Fatalf("PersistError %v", err)
	}
	// The container is unaffected; what is stored is a consistent older
	// checkpoint (its creation), never a record naming a missing blob.
	if c.Snapshot().State != Exited {
		t.Fatal(c.Snapshot())
	}
	m2 := manager(t, Options{Store: store})
	rep, _ := m2.Restore(lowerOf(img), restoreLimits)
	if rep.Restored != 1 || len(rep.Repaired) != 0 {
		t.Fatalf("%+v", rep)
	}
}

func TestLogsCodec(t *testing.T) {
	now := time.Unix(1700000000, 123)
	in := []Record{{Stream: Stdout, Data: []byte("out"), Time: now}, {Stream: Stderr, Data: []byte("err"), Time: now.Add(time.Second)}}
	out, err := decodeLogs(encodeLogs(in), 100)
	if err != nil || len(out) != 2 || string(out[1].Data) != "err" || !out[0].Time.Equal(now) || out[1].Stream != Stderr {
		t.Fatal(out, err)
	}
	if _, err := decodeLogs(encodeLogs(in), 5); !errors.Is(err, state.ErrCorrupt) {
		t.Fatal("over the bound accepted")
	}
	b := encodeLogs(in)
	for _, bad := range [][]byte{b[:len(b)-1], b[:len(logsMagic)+5], []byte("x"), append([]byte(logsMagic), 9, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 'x')} {
		if _, err := decodeLogs(bad, 100); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// FuzzDecodeRecord and FuzzDecodeLogs read what storage returns.
func FuzzDecodeRecord(f *testing.F) {
	raw, _ := json.Marshal(record{ID: strings.Repeat("a", 64), Name: "x", State: Exited, Entrypoint: []string{"p"}})
	f.Add(state.Seal(recordKind, raw))
	f.Add([]byte("wctr-state 1 container"))
	f.Fuzz(func(t *testing.T, v []byte) {
		r, err := DecodeRecord(v)
		if err == nil && r.check() != nil {
			t.Fatal("accepted a record that fails its checks")
		}
	})
}

func FuzzDecodeLogs(f *testing.F) {
	f.Add(encodeLogs([]Record{{Stream: Stdout, Data: []byte("x"), Time: time.Unix(1, 2)}}))
	f.Fuzz(func(t *testing.T, b []byte) {
		recs, err := decodeLogs(b, 1<<16)
		if err != nil {
			return
		}
		total := 0
		for _, r := range recs {
			total += len(r.Data)
		}
		if total > 1<<16 {
			t.Fatal("over the bound")
		}
	})
}
