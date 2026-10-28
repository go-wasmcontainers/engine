package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/rootfs"
)

// imagesLoad is POST /images/load: the body is an OCI image layout as a
// tar (what a builder exports); the reply streams {"stream": "Loaded
// image: …"} lines, as Docker's does.
func (s *server) imagesLoad(w http.ResponseWriter, r *http.Request) {
	loaded, err := s.d.Load(r.Context(), r.Body, nil)
	if err != nil && len(loaded) == 0 {
		var mb *http.MaxBytesError
		if errors.As(err, &mb) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Errorf("the layout is larger than %d bytes", mb.Limit))
			return
		}
		writeError(w, statusOf(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	for _, l := range loaded {
		if l.Name != "" {
			enc.Encode(api.JSONMessage{Stream: "Loaded image: " + l.Name + "\n"})
		} else {
			enc.Encode(api.JSONMessage{Stream: "Loaded image ID: " + string(l.ID) + "\n"})
		}
	}
	if err != nil {
		enc.Encode(api.JSONMessage{Error: err.Error(), ErrorDetail: &api.JSONMsgError{Message: err.Error()}})
	}
}

// archiveLimits bounds an archive upload into a container (its own quota
// applies on top).
var archiveLimits = rootfs.Limits{MaxBytes: 1 << 30, MaxFiles: 1 << 16, MaxPathLen: 4096, MaxDepth: 128}

// containersArchivePut is PUT /containers/{id}/archive?path=DIR: a tar
// extracted into the container's directory DIR.
func (s *server) containersArchivePut(w http.ResponseWriter, r *http.Request) {
	c, err := s.d.Containers().Get(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	dir := r.URL.Query().Get("path")
	if dir == "" {
		writeError(w, http.StatusBadRequest, errors.New("no path given"))
		return
	}
	noOverwrite := r.URL.Query().Get("noOverwriteDirNonDir") == "1" || strings.EqualFold(r.URL.Query().Get("noOverwriteDirNonDir"), "true")
	if err := c.FS().Extract(path.Clean("/"+dir), r.Body, archiveLimits, noOverwrite); err != nil {
		writeError(w, archiveStatus(err), err)
		return
	}
	s.d.Containers().Touched(c)
	w.WriteHeader(http.StatusOK)
}

// containersArchiveGet is GET /containers/{id}/archive?path=P: a tar of P
// rooted at its base name, with Docker's X-Docker-Container-Path-Stat
// header (base64 JSON of the path's stat).
func (s *server) containersArchiveGet(w http.ResponseWriter, r *http.Request) {
	c, p, ok := s.archiveStat(w, r)
	if !ok {
		return
	}
	w.Header().Set("Content-Type", "application/x-tar")
	w.WriteHeader(http.StatusOK)
	c.FS().WriteTar(p, w)
}

// containersArchiveHead is HEAD /containers/{id}/archive?path=P: the
// X-Docker-Container-Path-Stat header alone, without building the tar
// (a builder's WORKDIR and COPY checks).
func (s *server) containersArchiveHead(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := s.archiveStat(w, r); ok {
		w.WriteHeader(http.StatusOK)
	}
}

// archiveStat sets the X-Docker-Container-Path-Stat header (base64 JSON:
// name, size, mode, mtime, linkTarget, as the Engine API's
// ContainerPathStat), or answers the error.
func (s *server) archiveStat(w http.ResponseWriter, r *http.Request) (*container.Container, string, bool) {
	c, err := s.d.Containers().Get(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return nil, "", false
	}
	p := path.Clean("/" + r.URL.Query().Get("path"))
	in, err := c.FS().Lstat(p)
	if err != nil {
		writeError(w, archiveStatus(err), err)
		return nil, "", false
	}
	link := ""
	if in.Mode()&fs.ModeSymlink != 0 {
		link, _ = c.FS().Readlink(p)
	}
	stat, _ := json.Marshal(api.ContainerPathStat{Name: path.Base(p), Size: in.Size(), Mode: uint32(in.Mode()),
		Mtime: in.ModTime().UTC().Format(time.RFC3339Nano), LinkTarget: link})
	w.Header().Set("X-Docker-Container-Path-Stat", base64.StdEncoding.EncodeToString(stat))
	return c, p, true
}

func archiveStatus(err error) int {
	var mb *http.MaxBytesError
	switch {
	case errors.As(err, &mb):
		return http.StatusRequestEntityTooLarge
	case errors.Is(err, rootfs.ErrArchive), errors.Is(err, rootfs.ErrNotDir):
		return http.StatusBadRequest
	case errors.Is(err, rootfs.ErrNoSpace):
		return http.StatusInsufficientStorage
	case strings.Contains(err.Error(), "not exist"):
		return http.StatusNotFound
	}
	return statusOf(err)
}
