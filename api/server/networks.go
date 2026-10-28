package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/network"
)

// networkName is how the API names a container's network: the engine's
// default as "bridge".
func networkName(n string) string {
	if n == "" {
		return network.Bridge
	}
	return n
}

// networkOf is the network a container joins first, by name.
func networkOf(cfg container.Config) string { return networkName(cfg.PrimaryNetwork()) }

func (s *server) networkObject(n network.Network) api.Network {
	labels := n.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	out := api.Network{Name: n.Name, ID: n.ID, Created: n.Created.Format(time.RFC3339Nano), Scope: "local", Driver: n.Driver,
		IPAM: api.NetworkIPAM{Driver: "default", Config: []map[string]string{}}, Containers: map[string]api.NetworkEndpoint{},
		Options: map[string]string{}, Labels: labels, Attachable: true}
	if n.Subnet != "" {
		out.IPAM.Config = append(out.IPAM.Config, map[string]string{"Subnet": n.Subnet})
	}
	for _, c := range s.d.Containers().List(true) {
		for _, ni := range c.Networks {
			if networkName(ni.Network) == n.Name && ni.IPAddress.IsValid() {
				out.Containers[c.ID] = api.NetworkEndpoint{Name: c.Name, IPv4Address: ni.IPAddress.String() + "/" + prefixBits(n.Subnet)}
			}
		}
	}
	return out
}

// endpointSettings is inspect's NetworkSettings.Networks.
func (s *server) endpointSettings(sn container.Snapshot) map[string]api.EndpointSettings {
	out := map[string]api.EndpointSettings{}
	for _, ni := range sn.Networks {
		name := networkName(ni.Network)
		es := api.EndpointSettings{Aliases: ni.Aliases, DNSNames: append([]string{container.DNSName(sn.Name, sn.ID)}, ni.Aliases...)}
		if n, err := s.d.Networks().Get(name); err == nil {
			es.NetworkID = n.ID
			if ni.IPAddress.IsValid() {
				es.IPAddress = ni.IPAddress.String()
				es.IPPrefixLen, _ = strconv.Atoi(prefixBits(n.Subnet))
			}
		}
		out[name] = es
	}
	return out
}

func prefixBits(subnet string) string {
	for i := len(subnet) - 1; i >= 0; i-- {
		if subnet[i] == '/' {
			return subnet[i+1:]
		}
	}
	return "32"
}

func networkStatus(err error) int {
	switch {
	case errors.Is(err, network.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, network.ErrExists), errors.Is(err, network.ErrInUse):
		return http.StatusConflict
	case errors.Is(err, network.ErrInvalid):
		return http.StatusBadRequest
	}
	return statusOf(err)
}

// networksList honours the label, name, id and driver filters.
func (s *server) networksList(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilters(r.URL.Query().Get("filters"), "label", "name", "id", "driver")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	out := []api.Network{}
	for _, n := range s.d.Networks().List() {
		if !labelsMatch(n.Labels, f["label"]) ||
			!anyMatch(f["name"], func(v string) bool { return strings.Contains(n.Name, v) }) ||
			!anyMatch(f["id"], func(v string) bool { return strings.HasPrefix(n.ID, v) }) ||
			!anyMatch(f["driver"], func(v string) bool { return n.Driver == v }) {
			continue
		}
		out = append(out, s.networkObject(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func decodeBody(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		return fmt.Errorf("invalid body: %v", err)
	}
	return nil
}

func (s *server) networksCreate(w http.ResponseWriter, r *http.Request) {
	var req api.NetworkCreateRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if req.Driver != "" && req.Driver != "bridge" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("network driver %q: only bridge (in-engine) networks can be created", req.Driver))
		return
	}
	if req.Internal || len(req.Options) > 0 {
		writeError(w, http.StatusBadRequest, errors.New("network options are not supported"))
		return
	}
	n, err := s.d.NetworkCreate(req.Name, req.Labels)
	if err != nil {
		writeError(w, networkStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, api.NetworkCreateResponse{ID: n.ID})
}

func (s *server) networksInspect(w http.ResponseWriter, r *http.Request) {
	n, err := s.d.Networks().Get(r.PathValue("id"))
	if err != nil {
		writeError(w, networkStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.networkObject(n))
}

func (s *server) networksDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.d.NetworkRemove(r.PathValue("id")); err != nil {
		writeError(w, networkStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) networksConnect(w http.ResponseWriter, r *http.Request) {
	var req api.NetworkConnectRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var aliases []string
	if req.EndpointConfig != nil {
		aliases = req.EndpointConfig.Aliases
	}
	if err := s.d.NetworkConnect(context.WithoutCancel(r.Context()), r.PathValue("id"), req.Container, aliases); err != nil {
		writeError(w, networkStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) networksDisconnect(w http.ResponseWriter, r *http.Request) {
	var req api.NetworkDisconnectRequest
	if err := decodeBody(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.d.NetworkDisconnect(r.PathValue("id"), req.Container); err != nil {
		writeError(w, networkStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// networksPrune honours the label filter.
func (s *server) networksPrune(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilters(r.URL.Query().Get("filters"), "label")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	gone := s.d.NetworkPrune(func(labels map[string]string) bool { return labelsMatch(labels, f["label"]) })
	if gone == nil {
		gone = []string{}
	}
	writeJSON(w, http.StatusOK, api.NetworkPruneResponse{NetworksDeleted: gone})
}
