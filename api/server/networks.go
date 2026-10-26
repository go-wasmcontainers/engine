package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/network"
)

// networkOf is the network a container joins, by name.
func networkOf(cfg container.Config) string {
	if cfg.Network == "" {
		return network.Bridge
	}
	return cfg.Network
}

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
		if networkOf(c.Config) == n.Name && c.IPAddress.IsValid() {
			out.Containers[c.ID] = api.NetworkEndpoint{Name: c.Name, IPv4Address: c.IPAddress.String() + "/" + prefixBits(n.Subnet)}
		}
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

func (s *server) networksList(w http.ResponseWriter, r *http.Request) {
	out := []api.Network{}
	for _, n := range s.d.Networks().List() {
		out = append(out, s.networkObject(n))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) networksCreate(w http.ResponseWriter, r *http.Request) {
	var req api.NetworkCreateRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid body: %v", err))
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
	n, err := s.d.Networks().Create(req.Name, req.Labels)
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
