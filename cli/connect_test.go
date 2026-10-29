package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/internal/testutil"
)

func waitLog(t *testing.T, h, name, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !strings.Contains(wctr(t, h, "logs", name).stdout, want) {
		if time.Now().After(deadline) {
			t.Fatalf("%s: no %q in its logs", name, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// getJSON is a GET on the engine's API (for the filters the client does
// not expose).
func getJSON(t *testing.T, c *api.Client, path string, q url.Values, out any) int {
	t.Helper()
	hc, base := c.HTTP()
	resp, err := hc.Get(base + path + "?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusOK && out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			t.Fatalf("%s: %v\n%s", path, err, b)
		}
	}
	return resp.StatusCode
}

// TestNetworkConnect: aliases shared by replicas (a compose service's
// name), a container on two networks from its create (EndpointsConfig),
// connect and disconnect on a running container, prune, and the label
// filters compose lists its objects with.
func TestNetworkConnect(t *testing.T) {
	dir := shortDir(t)
	layout := filepath.Join(dir, "layout")
	testutil.WriteLayout(t, layout, testutil.BuildContent(
		testutil.ImageSpec{RefName: "netprobe", Module: testutil.Wasm(t, "netprobe"), ModulePath: "/bin/netprobe.wasm"},
	))
	h := netDaemon(t, dir)
	must(t, wctr(t, h, "pull", "oci:"+layout+":netprobe"), 0)
	must(t, wctr(t, h, "network", "create", "front"), 0)
	must(t, wctr(t, h, "network", "create", "--label", "com.docker.compose.project=demo", "back"), 0)
	must(t, wctr(t, h, "network", "create", "--label", "com.docker.compose.project=other", "spare"), 0)

	// Two replicas of one service: the alias answers both.
	for _, n := range []string{"api-1", "api-2"} {
		must(t, wctr(t, h, "run", "-d", "--name", n, "--network", "back", "--network-alias", "API", "netprobe", "serve", "80"), 0)
		waitLog(t, h, n, "listening")
	}
	r := must(t, wctr(t, h, "run", "--rm", "--network", "back", "netprobe", "lookup", "api"), 0)
	if strings.Count(r.stdout, "10.") != 2 {
		t.Fatalf("lookup api: %q, want both replicas", r.stdout)
	}
	if r := wctr(t, h, "run", "--rm", "--network-alias", "x", "netprobe", "lookup", "api"); r.code != 125 {
		t.Fatalf("an alias on the default network: %+v", r)
	}

	// Created on front (primary) and back: it reaches the service on back
	// by its alias, through its second network.
	c, err := api.NewClient(h)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	req := api.ContainerCreateRequest{NetworkingConfig: &api.NetworkingConfig{EndpointsConfig: map[string]*api.EndpointSettings{
		"front": {}, "back": {Aliases: []string{"client"}},
	}}}
	req.Image, req.Cmd = "netprobe", []string{"get", "http://api/two"}
	req.HostConfig.NetworkMode = "front"
	cr, err := c.Create(ctx, "both", req)
	if err != nil {
		t.Fatal(err)
	}
	must(t, wctr(t, h, "start", "-a", cr.ID), 0)
	if out := wctr(t, h, "logs", "both").stdout; !strings.Contains(out, "hello two from 10.90.") {
		t.Fatalf("two networks: %q", out)
	}
	raw := must(t, wctr(t, h, "inspect", "both"), 0).stdout
	var ins []api.ContainerInspect
	if err := json.Unmarshal([]byte(raw), &ins); err != nil || len(ins) != 1 {
		t.Fatalf("%v\n%s", err, raw)
	}
	nets := ins[0].NetworkSettings.Networks
	if ins[0].HostConfig.NetworkMode != "front" || len(nets) != 2 || len(nets["back"].Aliases) != 1 || nets["back"].Aliases[0] != "client" {
		t.Fatalf("inspect both: %+v", ins[0].NetworkSettings)
	}

	// Connected while running: its listener opens on the new network.
	must(t, wctr(t, h, "run", "-d", "--name", "gw", "--network", "front", "netprobe", "serve", "80"), 0)
	waitLog(t, h, "gw", "listening")
	if r := wctr(t, h, "run", "--rm", "--network", "back", "netprobe", "lookup", "gw"); r.code == 0 {
		t.Fatalf("gw resolved on back before connect: %+v", r)
	}
	must(t, wctr(t, h, "network", "connect", "--alias", "gateway", "back", "gw"), 0)
	if r := must(t, wctr(t, h, "run", "--rm", "--network", "back", "netprobe", "get", "http://gateway/live"), 0); !strings.Contains(r.stdout, "hello live from 10.90.") {
		t.Fatalf("after connect: %+v", r)
	}
	if r := wctr(t, h, "network", "connect", "back", "gw"); r.code != 1 || !strings.Contains(r.stderr, "already exists") {
		t.Fatalf("connect twice: %+v", r)
	}
	ns := must(t, wctr(t, h, "network", "inspect", "back"), 0).stdout
	if !strings.Contains(ns, `"gw"`) {
		t.Fatalf("network inspect back lacks gw:\n%s", ns)
	}
	// The network-scoped events.
	var evs []api.EventMessage
	err = c.Events(ctx, api.EventsOptions{Since: "1", Until: strconv.FormatInt(time.Now().Unix()+1, 10),
		Filters: map[string][]string{"type": {"network"}, "event": {"connect"}}}, func(e api.EventMessage) error {
		evs = append(evs, e)
		return nil
	})
	if err != nil || len(evs) == 0 || evs[len(evs)-1].Actor.Attributes["name"] != "back" || evs[len(evs)-1].Actor.Attributes["container"] == "" {
		t.Errorf("connect events: %+v, %v", evs, err)
	}

	// Disconnected: back no longer reaches it; front still does.
	must(t, wctr(t, h, "network", "disconnect", "back", "gw"), 0)
	if r := wctr(t, h, "run", "--rm", "--network", "back", "netprobe", "lookup", "gateway"); r.code == 0 {
		t.Fatalf("gateway resolves after disconnect: %+v", r)
	}
	if r := must(t, wctr(t, h, "run", "--rm", "--network", "front", "netprobe", "get", "http://gw/still"), 0); !strings.Contains(r.stdout, "hello still") {
		t.Fatalf("front after disconnect: %+v", r)
	}
	if r := wctr(t, h, "network", "disconnect", "back", "gw"); r.code != 1 || !strings.Contains(r.stderr, "not connected") {
		t.Fatalf("disconnect twice: %+v", r)
	}

	// Label filters.
	var list []api.Network
	if code := getJSON(t, c, "/networks", url.Values{"filters": {`{"label":["com.docker.compose.project=demo"]}`}}, &list); code != 200 || len(list) != 1 || list[0].Name != "back" {
		t.Fatalf("networks by label: %d %+v", code, list)
	}
	if code := getJSON(t, c, "/networks", url.Values{"filters": {`{"label":{"com.docker.compose.project":true}}`}}, &list); code != 200 || len(list) != 2 {
		t.Fatalf("networks by label (map form): %d %+v", code, list)
	}
	if code := getJSON(t, c, "/networks", url.Values{"filters": {`{"nope":["x"]}`}}, nil); code != 400 {
		t.Fatalf("an unknown filter: %d", code)
	}
	var cs []api.ContainerSummary
	if code := getJSON(t, c, "/containers/json", url.Values{"all": {"1"}, "filters": {`{"network":["back"],"status":["running"]}`}}, &cs); code != 200 || len(cs) != 2 {
		t.Fatalf("containers on back, running: %d %+v", code, cs)
	}
	// The same through the client's filtered lists.
	if cs, err := c.ContainersFiltered(ctx, true, api.Filters{"network": {"back"}, "status": {"running"}}); err != nil || len(cs) != 2 {
		t.Fatalf("Client.ContainersFiltered: %+v %v", cs, err)
	}
	if ns, err := c.NetworksFiltered(ctx, api.Filters{"label": {"com.docker.compose.project=demo"}}); err != nil || len(ns) != 1 || ns[0].Name != "back" {
		t.Fatalf("Client.NetworksFiltered: %+v %v", ns, err)
	}
	if vs, err := c.VolumesFiltered(ctx, api.Filters{"label": {"nobody"}}); err != nil || len(vs) != 0 {
		t.Fatalf("Client.VolumesFiltered: %+v %v", vs, err)
	}
	if _, err := c.NetworksFiltered(ctx, api.Filters{"nope": {"x"}}); err == nil {
		t.Fatal("an unknown filter through the client was accepted")
	}
	if ns, err := c.Networks(ctx); err != nil || len(ns) < 5 {
		t.Fatalf("Client.Networks without filters: %d %v", len(ns), err)
	}

	// Prune: the unused networks matching the filter, nothing in use.
	if r := must(t, wctr(t, h, "network", "prune", "-f", "--filter", "label=com.docker.compose.project=other"), 0); !strings.Contains(r.stdout, "spare") || strings.Contains(r.stdout, "back") {
		t.Fatalf("prune: %+v", r)
	}
	if r := wctr(t, h, "network", "prune", "-f", "--filter", "dangling=true"); r.code != 125 {
		t.Fatalf("prune with an unsupported filter: %+v", r)
	}
	must(t, wctr(t, h, "rm", "-f", "api-1", "api-2", "gw", "both"), 0)
	if r := must(t, wctr(t, h, "network", "prune", "-f"), 0); !strings.Contains(r.stdout, "back") || !strings.Contains(r.stdout, "front") {
		t.Fatalf("prune all: %+v", r)
	}
}
