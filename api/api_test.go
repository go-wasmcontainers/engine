package api_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/api/server"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/daemon"
	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
)

func TestStdcopy(t *testing.T) {
	var b bytes.Buffer
	if err := api.WriteFrame(&b, api.StreamStdout, []byte("out")); err != nil {
		t.Fatal(err)
	}
	api.WriteFrame(&b, api.StreamStderr, []byte("err"))
	api.WriteFrame(&b, api.StreamStdin, []byte("in"))
	raw := b.Bytes()
	if raw[0] != 1 || binary.BigEndian.Uint32(raw[4:8]) != 3 {
		t.Fatalf("header % x", raw[:8])
	}
	var o, e bytes.Buffer
	if err := api.Demux(&o, &e, bytes.NewReader(raw)); err != nil {
		t.Fatal(err)
	}
	if o.String() != "outin" || e.String() != "err" {
		t.Fatal(o.String(), e.String())
	}
	// A large payload is split into bounded frames and reassembled.
	big := bytes.Repeat([]byte("x"), 1<<24+10)
	b.Reset()
	api.WriteFrame(&b, api.StreamStdout, big)
	o.Reset()
	if err := api.Demux(&o, &e, &b); err != nil || o.Len() != len(big) {
		t.Fatal(err, o.Len())
	}
	for name, bad := range map[string][]byte{
		"short header": {1, 0, 0},
		"bad stream":   {9, 0, 0, 0, 0, 0, 0, 1, 'x'},
		"short body":   {1, 0, 0, 0, 0, 0, 0, 5, 'x'},
		"huge frame":   {1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff},
	} {
		if err := api.Demux(io.Discard, io.Discard, bytes.NewReader(bad)); !errors.Is(err, api.ErrBadFrame) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := api.WriteFrame(failWriter{}, 1, []byte("x")); err == nil {
		t.Fatal("write error lost")
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken") }

func TestStrSlice(t *testing.T) {
	var v struct{ A, B, C, D api.StrSlice }
	if err := json.Unmarshal([]byte(`{"A":"one","B":["x","y"],"C":null,"D":[]}`), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.A) != 1 || len(v.B) != 2 || v.C != nil || v.D == nil {
		t.Fatalf("%#v", v)
	}
	if err := json.Unmarshal([]byte(`{"A":3}`), &v); err == nil {
		t.Fatal("number accepted")
	}
}

func TestDefaultHost(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///x.sock")
	if api.DefaultHost() != "unix:///x.sock" {
		t.Fatal(api.DefaultHost())
	}
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1")
	if api.DefaultHost() != "unix:///run/user/1/wasmcontainers/dockerd.sock" {
		t.Fatal(api.DefaultHost())
	}
	t.Setenv("XDG_RUNTIME_DIR", "")
	if !strings.HasSuffix(api.DefaultSocket(), ".wasmcontainers/dockerd.sock") {
		t.Fatal(api.DefaultSocket())
	}
	if _, err := api.NewClient("tcp://1.2.3.4:2375"); err == nil {
		t.Fatal("tcp accepted")
	}
	if _, err := api.NewClient("unix://"); err == nil {
		t.Fatal("empty path accepted")
	}
}

func newServer(t *testing.T) (*httptest.Server, *api.Client, string) {
	e := wazero.New(wazero.Options{})
	t.Cleanup(func() { e.Close(context.Background()) })
	d, err := daemon.New(daemon.Options{Root: t.TempDir(), Containers: container.Options{Executor: e}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.New(d, server.Info{Version: "t"}))
	t.Cleanup(ts.Close)
	layout := filepath.Join(t.TempDir(), "l")
	testutil.WriteLayout(t, layout, testutil.BuildContent(
		testutil.ImageSpec{RefName: "probe", Module: testutil.Wasm(t, "probe"), ModulePath: "/bin/probe.wasm"}))
	return ts, api.NewClientWith(ts.Client(), ts.URL), layout
}

func get(t *testing.T, ts *httptest.Server, method, path, body string) (int, string) {
	req, _ := http.NewRequest(method, ts.URL+path, strings.NewReader(body))
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServerEdges(t *testing.T) {
	ts, c, layout := newServer(t)
	ctx := context.Background()
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		method, path, body string
		status             int
		contains           string
	}{
		{"GET", "/_ping", "", 200, "OK"},
		{"HEAD", "/_ping", "", 200, ""},
		{"GET", "/v1.24/_ping", "", 200, "OK"},
		{"GET", "/v1.12/_ping", "", 400, "not supported"},
		{"GET", "/v9.0/version", "", 400, "not supported"},
		{"GET", "/version", "", 200, `"ApiVersion":"1.47"`},
		{"POST", "/images/create", "", 400, "fromImage"},
		{"POST", "/images/create?fromImage=BAD!", "", 400, "invalid"},
		{"POST", "/images/create?fromImage=oci:/nope:x", "", 200, `"error"`},
		{"GET", "/images/nope/json", "", 404, "no such image"},
		{"GET", "/images/nope", "", 404, ""},
		{"DELETE", "/images/nope", "", 404, "no such image"},
		{"POST", "/containers/create", "{", 400, "invalid create body"},
		{"POST", "/containers/create", `{}`, 400, "no image"},
		{"POST", "/containers/create", `{"Image":"nope"}`, 404, "no such image"},
		{"POST", "/containers/create", strings.Repeat(" ", 2<<20), 400, ""},
		{"POST", "/containers/x/start", "", 404, "no such container"},
		{"POST", "/containers/x/wait", "", 404, "no such container"},
		{"POST", "/containers/x/wait?condition=sideways", "", 400, "invalid condition"},
		{"POST", "/containers/x/kill", "", 404, ""},
		{"GET", "/containers/x/logs", "", 400, "at least one stream"},
		{"GET", "/containers/x/logs?stdout=1", "", 404, ""},
		{"GET", "/containers/x/json", "", 404, ""},
		{"DELETE", "/containers/x", "", 404, ""},
		{"GET", "/containers/json?all=true", "", 200, "[]"},
		{"GET", "/images/json", "", 200, "[]"},
	} {
		status, body := get(t, ts, tc.method, tc.path, tc.body)
		if status != tc.status || !strings.Contains(body, tc.contains) {
			t.Errorf("%s %s: %d %q", tc.method, tc.path, status, body)
		}
	}
	// A pull through the client, then the shapes the CLI relies on.
	var msgs []string
	if err := c.Pull(ctx, "oci:"+layout+":probe", func(m api.JSONMessage) { msgs = append(msgs, m.Status) }); err != nil {
		t.Fatal(err)
	}
	if err := c.Pull(ctx, "oci:"+layout+":missing", nil); err == nil || !strings.Contains(err.Error(), "Error response from daemon") {
		t.Fatal(err)
	}
	imgs, err := c.Images(ctx)
	if err != nil || len(imgs) != 1 || imgs[0].SharedSize != -1 || imgs[0].RepoDigests == nil || imgs[0].Labels == nil {
		t.Fatalf("%+v %v", imgs, err)
	}
	// Cmd given as a JSON string, a TTY warning, a label.
	status, body := get(t, ts, "POST", "/containers/create?name=s", `{"Image":"probe","Cmd":"args","Tty":true,"Labels":{"a":"b"},"HostConfig":{"Memory":33554432}}`)
	if status != 201 || !strings.Contains(body, "TTY is not supported") {
		t.Fatal(status, body)
	}
	if status, _ = get(t, ts, "POST", "/containers/s/start", ""); status != 204 {
		t.Fatal(status)
	}
	w, err := c.Wait(ctx, "s", "")
	if err != nil {
		t.Fatal(err)
	}
	if res, err := w.Result(); err != nil || res.StatusCode != 0 {
		t.Fatal(res, err)
	}
	if status, _ = get(t, ts, "POST", "/containers/s/kill", ""); status != 409 {
		t.Fatal("kill of a stopped container:", status)
	}
	raw, err := c.Inspect(ctx, "s")
	if err != nil || !strings.Contains(string(raw), `"Memory":33554432`) || !strings.Contains(string(raw), `"Args":["args"]`) {
		t.Fatal(string(raw), err)
	}
	// Starting a running container: 304.
	get(t, ts, "POST", "/containers/create?name=spin", `{"Image":"probe","Cmd":["spin"]}`)
	get(t, ts, "POST", "/containers/spin/start", "")
	if status, _ = get(t, ts, "POST", "/containers/spin/start", ""); status != 304 {
		t.Fatal(status)
	}
	if status, body = get(t, ts, "DELETE", "/containers/spin", ""); status != 409 {
		t.Fatal(status, body)
	}
	cs, _ := c.Containers(ctx, false)
	if len(cs) != 1 || !strings.HasPrefix(cs[0].Status, "Up ") || cs[0].HostConfig.NetworkMode != "none" {
		t.Fatalf("%+v", cs)
	}
	// A wait that registers, then a kill: the wait answers 137.
	w, err = c.Wait(ctx, "spin", "next-exit")
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Kill(ctx, "spin"); err != nil {
		t.Fatal(err)
	}
	res, err := w.Result()
	if err != nil || res.StatusCode != 137 || res.Error == nil {
		t.Fatal(res, err)
	}
	if err := c.Remove(ctx, "spin", true); err != nil {
		t.Fatal(err)
	}
	// Logs stdout only.
	get(t, ts, "POST", "/containers/create?name=both", `{"Image":"probe","Cmd":["exit","0"]}`)
	get(t, ts, "POST", "/containers/both/start", "")
	ww, _ := c.Wait(ctx, "both", "")
	ww.Result()
	_, body = get(t, ts, "GET", "/containers/both/logs?stdout=1", "")
	if body != "" {
		t.Fatalf("stderr leaked into a stdout-only log: %q", body)
	}
	_, body = get(t, ts, "GET", "/containers/both/logs?stderr=1", "")
	if !strings.HasSuffix(body, "bye\n") {
		t.Fatalf("%q", body)
	}
	// Remove the image by ID, forced, while a container uses it.
	if _, err := c.RemoveImage(ctx, imgs[0].ID[7:19], false); err == nil {
		t.Fatal("removed an image in use")
	}
	items, err := c.RemoveImage(ctx, imgs[0].ID, true)
	if err != nil || len(items) < 2 {
		t.Fatal(items, err)
	}
	// The daemon reports a non-JSON error body as text.
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Error(w, "teapot", 418) }))
	defer plain.Close()
	pc := api.NewClientWith(plain.Client(), plain.URL)
	if err := pc.Ping(ctx); err == nil || err.Error() != "Error response from daemon: teapot" {
		t.Fatal(err)
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer empty.Close()
	if err := api.NewClientWith(empty.Client(), empty.URL).Ping(ctx); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatal(err)
	}
	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("not json\n")) }))
	defer junk.Close()
	if err := api.NewClientWith(junk.Client(), junk.URL).Pull(ctx, "x", nil); err == nil {
		t.Fatal("junk progress accepted")
	}
	_ = time.Second
}
