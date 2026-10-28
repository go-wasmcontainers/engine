package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/daemon"
	"github.com/go-wasmcontainers/engine/image"
)

func toAPIConfig(c image.ExecConfig) api.ImageConfig {
	out := api.ImageConfig{User: c.User, ExposedPorts: c.ExposedPorts, Env: c.Env, Cmd: c.Cmd, Entrypoint: c.Entrypoint,
		Volumes: c.Volumes, WorkingDir: c.WorkingDir, Labels: c.Labels, StopSignal: c.StopSignal, ArgsEscaped: c.ArgsEscaped,
		Shell: c.Shell, OnBuild: c.OnBuild}
	if h := c.Healthcheck; h != nil {
		out.Healthcheck = &api.HealthConfig{Test: h.Test, Interval: h.Interval, Timeout: h.Timeout, StartPeriod: h.StartPeriod,
			StartInterval: h.StartInterval, Retries: h.Retries}
	}
	return out
}

func fromAPIConfig(c api.ImageConfig) image.ExecConfig {
	out := image.ExecConfig{User: c.User, ExposedPorts: c.ExposedPorts, Env: c.Env, Cmd: c.Cmd, Entrypoint: c.Entrypoint,
		Volumes: c.Volumes, WorkingDir: c.WorkingDir, Labels: c.Labels, StopSignal: c.StopSignal, ArgsEscaped: c.ArgsEscaped,
		Shell: c.Shell, OnBuild: c.OnBuild}
	if h := c.Healthcheck; h != nil {
		out.Healthcheck = &image.HealthConfig{Test: h.Test, Interval: h.Interval, Timeout: h.Timeout, StartPeriod: h.StartPeriod,
			StartInterval: h.StartInterval, Retries: h.Retries}
	}
	return out
}

// commit is POST /commit?container=&repo=&tag=&comment=&author=, the body
// the image config (empty: the container's own).
func (s *server) commit(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req := daemon.CommitRequest{Container: q.Get("container"), Repo: q.Get("repo"), Tag: q.Get("tag"),
		Comment: q.Get("comment"), Author: q.Get("author"), Changes: q["changes"]}
	if req.Container == "" {
		writeError(w, http.StatusBadRequest, errors.New("no container given"))
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20+1))
	if err != nil || len(body) > 1<<20 {
		writeError(w, http.StatusBadRequest, errors.New("invalid or too large config"))
		return
	}
	if len(strings.TrimSpace(string(body))) > 0 && strings.TrimSpace(string(body)) != "null" {
		var c api.ImageConfig
		if err := json.Unmarshal(body, &c); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid config: %v", err))
			return
		}
		ec := fromAPIConfig(c)
		req.Config = &ec
	}
	res, err := s.d.Commit(req)
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, api.CommitResponse{ID: string(res.ID)})
}

// imagesPost serves POST /images/{name}/tag (names hold slashes, so the
// action is the last path element).
func (s *server) imagesPost(w http.ResponseWriter, r *http.Request) {
	rest := r.PathValue("rest")
	name, ok := strings.CutSuffix(rest, "/tag")
	if !ok || name == "" {
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	if q.Get("repo") == "" {
		writeError(w, http.StatusBadRequest, errors.New("no repository given"))
		return
	}
	if _, err := s.d.TagImage(name, q.Get("repo"), q.Get("tag")); err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	w.WriteHeader(http.StatusCreated)
}
