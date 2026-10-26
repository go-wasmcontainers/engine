package server

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/container"
)

// StatsInterval is how often a stats stream reads (Docker's: 1 s); tests
// shorten it.
var StatsInterval = time.Second

func (s *server) statsOf(c *container.Container, prev string) api.ContainerStats {
	st := c.Stats()
	sn := c.Snapshot()
	if st.MemoryLimitBytes <= 0 {
		st.MemoryLimitBytes = s.d.Containers().DefaultMemoryLimit()
	}
	out := api.ContainerStats{
		Read: st.Read.Format(time.RFC3339Nano), Preread: prev, Name: "/" + sn.Name, ID: sn.ID,
		MemoryStats: api.MemoryStats{Usage: uint64(max(st.MemoryBytes, 0)), Limit: uint64(max(st.MemoryLimitBytes, 0))},
		BlkioStats: api.BlkioStats{IOServiceBytesRecursive: []api.BlkioEntry{
			{Op: "read", Value: 0}, {Op: "write", Value: uint64(max(st.WrittenBytes, 0))},
		}},
		Wctr: api.WctrStatsInfo{Running: st.Running, SizeRw: st.SizeRw, LogBytes: st.LogBytes,
			CPU: "not measured: a WebAssembly guest gives the engine no CPU accounting"},
	}
	if prev == "" {
		out.Preread = "0001-01-01T00:00:00Z"
	}
	if st.Running {
		out.PidsStats.Current = 1
	}
	return out
}

// containersStats answers one reading (stream=0) or one per second until
// the client leaves or the container is removed.
func (s *server) containersStats(w http.ResponseWriter, r *http.Request) {
	c, err := s.d.Containers().Get(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	stream := r.URL.Query().Get("stream") == "" || boolParam(r, "stream")
	w.Header().Set("Content-Type", "application/json")
	if !stream {
		writeJSON(w, http.StatusOK, s.statsOf(c, ""))
		return
	}
	w.WriteHeader(http.StatusOK)
	fw := newFlushWriter(w)
	enc := json.NewEncoder(fw)
	t := time.NewTicker(StatsInterval)
	defer t.Stop()
	prev := ""
	for {
		st := s.statsOf(c, prev)
		if enc.Encode(st) != nil {
			return
		}
		prev = st.Read
		select {
		case <-t.C:
		case <-r.Context().Done():
			return
		}
		if _, err := s.d.Containers().Get(c.ID()); err != nil {
			return // removed
		}
	}
}
