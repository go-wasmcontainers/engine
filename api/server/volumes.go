package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/volume"
)

func (s *server) volumeObject(v *volume.Volume, usage bool) api.Volume {
	labels := v.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	out := api.Volume{Name: v.Name, Driver: "local", Mountpoint: "volume:" + v.Name, CreatedAt: v.CreatedAt.Format(time.RFC3339),
		Labels: labels, Scope: "local", Options: map[string]string{}}
	if usage {
		out.UsageData = &api.VolumeUsage{Size: v.Size(), RefCount: int64(len(s.d.Containers().UsesVolume(v.Name)))}
	}
	return out
}

func (s *server) volumeList(usage bool) []api.Volume {
	out := []api.Volume{}
	for _, v := range s.d.Volumes().List() {
		out = append(out, s.volumeObject(v, usage))
	}
	return out
}

func volumeStatus(err error) int {
	switch {
	case errors.Is(err, volume.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, volume.ErrInUse):
		return http.StatusConflict
	case errors.Is(err, volume.ErrInvalid), errors.Is(err, volume.ErrTooMany):
		return http.StatusBadRequest
	}
	return statusOf(err)
}

// volumesList honours the name and dangling filters; others are refused.
func (s *server) volumesList(w http.ResponseWriter, r *http.Request) {
	var filters map[string][]string
	if f := r.URL.Query().Get("filters"); f != "" {
		if err := json.Unmarshal([]byte(f), &filters); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid filters: %v", err))
			return
		}
		for k := range filters {
			if k != "name" && k != "dangling" {
				writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported volume filter %q", k))
				return
			}
		}
	}
	out := api.VolumeListResponse{Volumes: []api.Volume{}, Warnings: []string{}}
	for _, v := range s.d.Volumes().List() {
		if names := filters["name"]; len(names) > 0 && !anyContains(v.Name, names) {
			continue
		}
		if d := filters["dangling"]; len(d) > 0 {
			dangling := len(s.d.Containers().UsesVolume(v.Name)) == 0
			if want := d[0] == "true" || d[0] == "1"; want != dangling {
				continue
			}
		}
		out.Volumes = append(out.Volumes, s.volumeObject(v, false))
	}
	writeJSON(w, http.StatusOK, out)
}

func anyContains(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func (s *server) volumesCreate(w http.ResponseWriter, r *http.Request) {
	var req api.VolumeCreateOptions
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid body: %v", err))
		return
	}
	if req.Driver != "" && req.Driver != "local" {
		writeError(w, http.StatusBadRequest, fmt.Errorf("volume driver %q: only local volumes are supported", req.Driver))
		return
	}
	if len(req.DriverOpts) > 0 {
		writeError(w, http.StatusBadRequest, errors.New("driver options are not supported"))
		return
	}
	v, err := s.d.CreateVolume(req.Name, req.Labels)
	if err != nil {
		writeError(w, volumeStatus(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, s.volumeObject(v, false))
}

func (s *server) volumesInspect(w http.ResponseWriter, r *http.Request) {
	v, err := s.d.Volumes().Get(r.PathValue("name"))
	if err != nil {
		writeError(w, volumeStatus(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.volumeObject(v, false))
}

func (s *server) volumesDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.d.RemoveVolume(r.PathValue("name"), boolParam(r, "force")); err != nil {
		writeError(w, volumeStatus(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// volumesPrune removes unused anonymous volumes, or every unused one with
// the all=true filter.
func (s *server) volumesPrune(w http.ResponseWriter, r *http.Request) {
	all := false
	if f := r.URL.Query().Get("filters"); f != "" {
		var filters map[string][]string
		if err := json.Unmarshal([]byte(f), &filters); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid filters: %v", err))
			return
		}
		for k, v := range filters {
			if k != "all" || len(v) != 1 {
				writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported prune filter %q", k))
				return
			}
			all = v[0] == "true" || v[0] == "1"
		}
	}
	gone, n := s.d.PruneVolumes(all)
	if gone == nil {
		gone = []string{}
	}
	writeJSON(w, http.StatusOK, api.VolumePruneResponse{VolumesDeleted: gone, SpaceReclaimed: n})
}
