// Package server serves the engine's Docker Engine API subset over HTTP.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	units "github.com/docker/go-units"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/daemon"
	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/portmap"
	"github.com/go-wasmcontainers/engine/runtime"
)

// maxBody bounds a request body (a container create is a few hundred bytes).
const maxBody = 1 << 20

// Info is what /version reports about the build.
type Info struct {
	Version   string
	GitCommit string
	// MaxUploadBytes bounds POST /images/load and PUT
	// /containers/{id}/archive bodies (default DefaultMaxUploadBytes);
	// every other request body is bounded at 1 MiB.
	MaxUploadBytes int64
}

type server struct {
	d    *daemon.Daemon
	info Info
	mux  *http.ServeMux
}

var versionPrefix = regexp.MustCompile(`^/v([0-9]+)\.([0-9]+)(/.*)$`)

// New returns the API handler for d.
func New(d *daemon.Daemon, info Info) http.Handler {
	s := &server{d: d, info: info, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /_ping", s.ping)
	s.mux.HandleFunc("HEAD /_ping", s.ping)
	s.mux.HandleFunc("GET /version", s.version)
	s.mux.HandleFunc("POST /images/create", s.imagesCreate)
	s.mux.HandleFunc("GET /images/json", s.imagesList)
	s.mux.HandleFunc("GET /images/{name...}", s.imagesInspect)
	s.mux.HandleFunc("DELETE /images/{name...}", s.imagesDelete)
	s.mux.HandleFunc("POST /containers/create", s.containersCreate)
	s.mux.HandleFunc("GET /containers/json", s.containersList)
	s.mux.HandleFunc("POST /containers/{id}/start", s.containersStart)
	s.mux.HandleFunc("POST /containers/{id}/wait", s.containersWait)
	s.mux.HandleFunc("POST /containers/{id}/kill", s.containersKill)
	s.mux.HandleFunc("GET /containers/{id}/logs", s.containersLogs)
	s.mux.HandleFunc("GET /containers/{id}/stats", s.containersStats)
	s.mux.HandleFunc("GET /containers/{id}/json", s.containersInspect)
	s.mux.HandleFunc("DELETE /containers/{id}", s.containersDelete)
	s.mux.HandleFunc("POST /containers/prune", s.containersPrune)
	s.mux.HandleFunc("GET /events", s.systemEvents)
	s.mux.HandleFunc("GET /volumes", s.volumesList)
	s.mux.HandleFunc("POST /volumes/create", s.volumesCreate)
	s.mux.HandleFunc("POST /volumes/prune", s.volumesPrune)
	s.mux.HandleFunc("GET /volumes/{name}", s.volumesInspect)
	s.mux.HandleFunc("DELETE /volumes/{name}", s.volumesDelete)
	s.mux.HandleFunc("POST /containers/{id}/stop", s.containersStop)
	s.mux.HandleFunc("POST /containers/{id}/restart", s.containersRestart)
	s.mux.HandleFunc("POST /containers/{id}/attach", s.containersAttach)
	s.mux.HandleFunc("POST /containers/{id}/resize", s.containersResize)
	s.mux.HandleFunc("POST /containers/{id}/exec", s.containersExec)
	s.mux.HandleFunc("POST /exec/{id}/start", s.execStart)
	s.mux.HandleFunc("POST /exec/{id}/resize", s.execResize)
	s.mux.HandleFunc("GET /exec/{id}/json", s.execInspect)
	s.mux.HandleFunc("GET /containers/{id}/changes", s.containersChanges)
	s.mux.HandleFunc("POST /images/prune", s.imagesPrune)
	s.mux.HandleFunc("GET /system/df", s.systemDF)
	s.mux.HandleFunc("POST /commit", s.commit)
	s.mux.HandleFunc("POST /images/load", s.imagesLoad)
	s.mux.HandleFunc("PUT /containers/{id}/archive", s.containersArchivePut)
	s.mux.HandleFunc("GET /containers/{id}/archive", s.containersArchiveGet)
	s.mux.HandleFunc("HEAD /containers/{id}/archive", s.containersArchiveHead)
	s.mux.HandleFunc("POST /images/{rest...}", s.imagesPost)
	s.mux.HandleFunc("GET /networks", s.networksList)
	s.mux.HandleFunc("POST /networks/create", s.networksCreate)
	s.mux.HandleFunc("GET /networks/{id}", s.networksInspect)
	s.mux.HandleFunc("DELETE /networks/{id}", s.networksDelete)
	s.mux.HandleFunc("POST /networks/{id}/connect", s.networksConnect)
	s.mux.HandleFunc("POST /networks/{id}/disconnect", s.networksDisconnect)
	s.mux.HandleFunc("POST /networks/prune", s.networksPrune)
	return s
}

func (s *server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Accept /vX.Y/... for any version from MinAPIVersion up to APIVersion.
	if m := versionPrefix.FindStringSubmatch(r.URL.Path); m != nil {
		if !versionInRange(m[1], m[2]) {
			writeError(w, http.StatusBadRequest, fmt.Errorf("client version %s.%s is not supported (%s to %s)", m[1], m[2], api.MinAPIVersion, api.APIVersion))
			return
		}
		r2 := r.Clone(r.Context())
		r2.URL.Path = m[3]
		r2.URL.RawPath = ""
		r = r2
	}
	w.Header().Set("Api-Version", api.APIVersion)
	w.Header().Set("Server", "go-wasmcontainers-engine/"+s.info.Version)
	r.Body = http.MaxBytesReader(w, r.Body, s.bodyLimit(r))
	s.mux.ServeHTTP(w, r)
}

// DefaultMaxUploadBytes bounds an image load or an archive upload when
// Info.MaxUploadBytes is not set.
const DefaultMaxUploadBytes = 1 << 30

// bodyLimit is a request's body bound: 1 MiB, except for the two routes
// that upload content (an image layout, a container archive).
func (s *server) bodyLimit(r *http.Request) int64 {
	upload := r.Method == http.MethodPost && r.URL.Path == "/images/load" ||
		r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/containers/") && strings.HasSuffix(r.URL.Path, "/archive") ||
		// An exec's stdin follows its start object in the body.
		r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/exec/") && strings.HasSuffix(r.URL.Path, "/start")
	if !upload {
		return maxBody
	}
	if s.info.MaxUploadBytes > 0 {
		return s.info.MaxUploadBytes
	}
	return DefaultMaxUploadBytes
}

func versionInRange(maj, min string) bool {
	v := func(a, b string) int {
		x, _ := strconv.Atoi(a)
		y, _ := strconv.Atoi(b)
		return x*1000 + y
	}
	split := func(s string) (string, string) { a, b, _ := strings.Cut(s, "."); return a, b }
	got := v(maj, min)
	lo := v(split(api.MinAPIVersion))
	hi := v(split(api.APIVersion))
	return got >= lo && got <= hi
}

// humanDuration is Docker's own rendering (github.com/docker/go-units).
func humanDuration(d time.Duration) string { return units.HumanDuration(d) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, api.ErrorResponse{Message: err.Error()})
}

// statusOf maps engine errors to the statuses the Docker API documents.
func statusOf(err error) int {
	switch {
	case errors.Is(err, container.ErrNotFound), errors.Is(err, daemon.ErrNoSuchImage), errors.Is(err, image.ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, container.ErrNameConflict), errors.Is(err, container.ErrRunning), errors.Is(err, daemon.ErrImageInUse):
		return http.StatusConflict
	case errors.Is(err, container.ErrInvalid), errors.Is(err, container.ErrAmbiguous), errors.Is(err, runtime.ErrNotFound),
		errors.Is(err, runtime.ErrInvalid), errors.Is(err, container.ErrTooMany):
		return http.StatusBadRequest
	case errors.Is(err, container.ErrNotRunning), errors.Is(err, container.ErrExecStarted),
		errors.Is(err, container.ErrNoStdin), errors.Is(err, container.ErrNoTerminal),
		errors.Is(err, container.ErrExecNotRunning), errors.Is(err, runtime.ErrNoExec):
		return http.StatusConflict
	case errors.Is(err, container.ErrNoSuchExec):
		return http.StatusNotFound
	}
	return http.StatusInternalServerError
}

func boolParam(r *http.Request, name string) bool {
	v := r.URL.Query().Get(name)
	return v == "1" || strings.EqualFold(v, "true")
}

func (s *server) ping(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Docker-Experimental", "false")
	w.Header().Set("Swarm", "inactive")
	w.Header().Set("Builder-Version", "")
	if r.Method == http.MethodHead {
		return
	}
	io.WriteString(w, "OK")
}

func (s *server) version(w http.ResponseWriter, r *http.Request) {
	v := api.SystemVersion{Version: s.info.Version, APIVersion: api.APIVersion, MinAPIVersion: api.MinAPIVersion,
		GitCommit: s.info.GitCommit, GoVersion: goruntime.Version(), Os: goruntime.GOOS, Arch: goruntime.GOARCH,
		KernelVersion: "wasip1"}
	v.Platform.Name = "go-wasmcontainers engine (WASI preview 1 containers)"
	writeJSON(w, http.StatusOK, v)
}

type flushWriter struct {
	w  http.ResponseWriter
	rc *http.ResponseController
}

func (f flushWriter) Write(p []byte) (int, error) {
	n, err := f.w.Write(p)
	if err == nil {
		f.rc.Flush()
	}
	return n, err
}

func newFlushWriter(w http.ResponseWriter) flushWriter {
	return flushWriter{w: w, rc: http.NewResponseController(w)}
}

func (s *server) imagesCreate(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	from := q.Get("fromImage")
	if from == "" {
		writeError(w, http.StatusBadRequest, errors.New("only pulls are supported: fromImage is required (import with fromSrc is not)"))
		return
	}
	if tag := q.Get("tag"); tag != "" && !image.IsLayoutRef(from) {
		sep := ":"
		if strings.HasPrefix(tag, "sha256:") {
			sep = "@"
		}
		from += sep + tag
	}
	if _, err := image.ParseReference(from); err != nil && !image.IsLayoutRef(from) {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	fw := newFlushWriter(w)
	enc := json.NewEncoder(fw)
	enc.Encode(api.JSONMessage{Status: "Pulling from " + from})
	name, res, err := s.d.Pull(r.Context(), from, func(id, status string) {
		enc.Encode(api.JSONMessage{ID: id, Status: status})
	})
	if err != nil {
		enc.Encode(api.JSONMessage{Error: err.Error(), ErrorDetail: &api.JSONMsgError{Message: err.Error()}})
		return
	}
	enc.Encode(api.JSONMessage{Status: "Digest: " + string(res.RepoDigest)})
	if res.Downloaded {
		enc.Encode(api.JSONMessage{Status: "Status: Downloaded newer image for " + name})
	} else {
		enc.Encode(api.JSONMessage{Status: "Status: Image is up to date for " + name})
	}
}

func (s *server) imagesList(w http.ResponseWriter, r *http.Request) {
	imgs, err := s.d.Images()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, imageSummaries(imgs))
}

func (s *server) systemDF(w http.ResponseWriter, r *http.Request) {
	u, err := s.d.DiskUsage()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	st := u.Storage
	writeJSON(w, http.StatusOK, api.SystemDataUsage{
		LayersSize: u.LayersSize, Images: imageSummaries(u.Images), Containers: containerSummaries(u.Containers),
		Volumes: s.volumeList(true), BuildCache: []any{},
		Storage: &api.StorageUsage{Kind: st.Kind, Usage: st.Usage, Quota: st.Quota, Persistent: st.Persistent, Error: st.Error},
	})
}

func (s *server) containersChanges(w http.ResponseWriter, r *http.Request) {
	c, err := s.d.Containers().Get(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	out := []api.FilesystemChange{}
	for _, ch := range c.FS().Diff() {
		out = append(out, api.FilesystemChange{Path: ch.Path, Kind: ch.Kind})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) containersPrune(w http.ResponseWriter, r *http.Request) {
	ids, n := s.d.Containers().Prune()
	if ids == nil {
		ids = []string{}
	}
	writeJSON(w, http.StatusOK, api.ContainerPruneResponse{ContainersDeleted: ids, SpaceReclaimed: n})
}

// imagesPrune honours the dangling filter (all unused images when it is
// false); other filters are refused rather than ignored.
func (s *server) imagesPrune(w http.ResponseWriter, r *http.Request) {
	all := false
	if f := r.URL.Query().Get("filters"); f != "" {
		var filters map[string][]string
		if err := json.Unmarshal([]byte(f), &filters); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("invalid filters: %v", err))
			return
		}
		for k, v := range filters {
			if k != "dangling" || len(v) != 1 {
				writeError(w, http.StatusBadRequest, fmt.Errorf("unsupported prune filter %q", k))
				return
			}
			switch v[0] {
			case "false", "0":
				all = true
			case "true", "1":
			default:
				writeError(w, http.StatusBadRequest, fmt.Errorf("invalid dangling value %q", v[0]))
				return
			}
		}
	}
	items, n, err := s.d.PruneImages(all)
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	out := make([]api.ImageDeleteResponseItem, len(items))
	for i, it := range items {
		out[i] = api.ImageDeleteResponseItem{Untagged: it.Untagged, Deleted: it.Deleted}
	}
	writeJSON(w, http.StatusOK, api.ImagePruneResponse{ImagesDeleted: out, SpaceReclaimed: n})
}

func imageSummaries(imgs []daemon.ImageInfo) []api.ImageSummary {
	out := make([]api.ImageSummary, 0, len(imgs))
	for _, i := range imgs {
		labels := i.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		rd := i.RepoDigests
		if rd == nil {
			rd = []string{}
		}
		out = append(out, api.ImageSummary{ID: string(i.ID), RepoTags: i.RepoTags, RepoDigests: rd, Created: i.Created.Unix(),
			Size: i.Size, SharedSize: -1, Labels: labels, Containers: int64(i.Containers)})
	}
	return out
}

func (s *server) imagesInspect(w http.ResponseWriter, r *http.Request) {
	ref, ok := strings.CutSuffix(r.PathValue("name"), "/json")
	if !ok {
		http.NotFound(w, r)
		return
	}
	_, img, err := s.d.ResolveImage(ref)
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	imgs, _ := s.d.Images()
	var tags, digests []string
	for _, i := range imgs {
		if i.ID == img.ID() {
			tags, digests = i.RepoTags, i.RepoDigests
		}
	}
	ec := daemon.ExecConfig(img)
	if tags == nil {
		tags = []string{}
	}
	if digests == nil {
		digests = []string{}
	}
	out := api.ImageInspect{ID: string(img.ID()), RepoTags: tags, RepoDigests: digests, Created: img.Config.Created,
		Author: img.Config.Author, Architecture: img.Config.Architecture, Os: img.Config.OS, Size: img.Size,
		Config: toAPIConfig(ec),
		// diff_ids: the layers' uncompressed digests, as Docker lists them.
		RootFS: map[string]any{"Type": "layers", "Layers": img.DiffIDs()}}
	for _, h := range img.Config.History {
		out.History = append(out.History, api.ImageHistory{Created: h.Created, CreatedBy: h.CreatedBy, Author: h.Author,
			Comment: h.Comment, EmptyLayer: h.EmptyLayer})
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) imagesDelete(w http.ResponseWriter, r *http.Request) {
	items, err := s.d.RemoveImage(r.PathValue("name"), boolParam(r, "force"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	out := make([]api.ImageDeleteResponseItem, len(items))
	for i, it := range items {
		out[i] = api.ImageDeleteResponseItem{Untagged: it.Untagged, Deleted: it.Deleted}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) containersCreate(w http.ResponseWriter, r *http.Request) {
	var req api.ContainerCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid create body: %v", err))
		return
	}
	if req.Image == "" {
		writeError(w, http.StatusBadRequest, errors.New("no image specified"))
		return
	}
	caps, err := api.DeviceCaps(req.HostConfig.DeviceRequests)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ports, err := portmap.FromAPI(req.HostConfig.PortBindings)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var warnings []string
	var console [2]uint16
	switch n := len(req.HostConfig.ConsoleSize); {
	case n == 2:
		console = [2]uint16{req.HostConfig.ConsoleSize[0], req.HostConfig.ConsoleSize[1]}
	case n != 0:
		writeError(w, http.StatusBadRequest, errors.New("ConsoleSize is [height, width]"))
		return
	}
	var endpoints map[string][]string
	if req.NetworkingConfig != nil {
		for name, es := range req.NetworkingConfig.EndpointsConfig {
			if endpoints == nil {
				endpoints = map[string][]string{}
			}
			if es != nil {
				endpoints[name] = es.Aliases
			} else {
				endpoints[name] = nil
			}
		}
	}
	var restart container.RestartPolicy
	if rp := req.HostConfig.RestartPolicy; rp != nil {
		restart = container.RestartPolicy{Name: rp.Name, MaximumRetryCount: rp.MaximumRetryCount}
	}
	c, err := s.d.CreateContainer(daemon.CreateRequest{
		Name: r.URL.Query().Get("name"), Image: req.Image,
		Entrypoint: req.Entrypoint, Cmd: req.Cmd, Env: req.Env, WorkingDir: req.WorkingDir, Labels: req.Labels,
		Memory: req.HostConfig.Memory, AutoRemove: req.HostConfig.AutoRemove, Binds: req.HostConfig.Binds,
		Network: req.HostConfig.NetworkMode, Endpoints: endpoints, Caps: caps,
		Ports: ports, ExposedPorts: req.ExposedPorts, PublishAll: req.HostConfig.PublishAllPorts,
		Restart: restart, Healthcheck: healthFromAPI(req.Healthcheck),
		Tty: req.Tty, OpenStdin: req.OpenStdin, StdinOnce: req.StdinOnce, ConsoleSize: console,
	})
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	if warnings == nil {
		warnings = []string{}
	}
	writeJSON(w, http.StatusCreated, api.ContainerCreateResponse{ID: c.ID(), Warnings: warnings})
}

// healthFromAPI reads Config.Healthcheck (durations in nanoseconds).
func healthFromAPI(h *api.HealthConfig) *container.HealthConfig {
	if h == nil {
		return nil
	}
	return &container.HealthConfig{Test: h.Test, Interval: time.Duration(h.Interval), Timeout: time.Duration(h.Timeout),
		StartPeriod: time.Duration(h.StartPeriod), StartInterval: time.Duration(h.StartInterval), Retries: h.Retries}
}

// healthToAPI is inspect's Config.Healthcheck.
func healthToAPI(h *container.HealthConfig) *api.HealthConfig {
	if h == nil {
		return nil
	}
	return &api.HealthConfig{Test: h.Test, Interval: int64(h.Interval), Timeout: int64(h.Timeout),
		StartPeriod: int64(h.StartPeriod), StartInterval: int64(h.StartInterval), Retries: h.Retries}
}

// healthState is inspect's State.Health.
func healthState(h *container.Health) *api.Health {
	if h == nil {
		return nil
	}
	out := &api.Health{Status: h.Status, FailingStreak: h.FailingStreak, Log: []api.HealthcheckResult{}}
	for _, r := range h.Log {
		out.Log = append(out.Log, api.HealthcheckResult{Start: rfc3339(r.Start), End: rfc3339(r.End), ExitCode: r.ExitCode, Output: r.Output})
	}
	return out
}

// humanStatus renders the STATUS column the way Docker does.
func humanStatus(s container.Snapshot, now time.Time) string {
	switch s.State {
	case container.Running:
		up := "Up " + humanDuration(now.Sub(s.StartedAt))
		switch {
		case s.Health == nil:
		case s.Health.Status == container.HealthStarting:
			up += " (health: starting)"
		default:
			up += " (" + s.Health.Status + ")"
		}
		return up
	case container.Exited:
		return fmt.Sprintf("Exited (%d) %s ago", s.ExitCode, humanDuration(now.Sub(s.FinishedAt)))
	case container.Restarting:
		return fmt.Sprintf("Restarting (%d) %s ago", s.ExitCode, humanDuration(now.Sub(s.FinishedAt)))
	}
	return "Created"
}

// containersList honours the label, name, id, status and network filters.
func (s *server) containersList(w http.ResponseWriter, r *http.Request) {
	f, err := parseFilters(r.URL.Query().Get("filters"), "label", "name", "id", "status", "network")
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	all := boolParam(r, "all") || len(f["status"]) > 0
	var list []container.Snapshot
	for _, c := range s.d.Containers().List(all) {
		if !labelsMatch(c.Config.Labels, f["label"]) ||
			!anyMatch(f["name"], func(v string) bool { return strings.Contains(c.Name, strings.TrimPrefix(v, "/")) }) ||
			!anyMatch(f["id"], func(v string) bool { return strings.HasPrefix(c.ID, v) }) ||
			!anyMatch(f["status"], func(v string) bool { return string(c.State) == v }) ||
			!anyMatch(f["network"], func(v string) bool {
				return slices.ContainsFunc(c.Networks, func(ni container.NetworkInfo) bool {
					if n, err := s.d.Networks().Get(v); err == nil {
						return networkName(ni.Network) == n.Name
					}
					return false
				})
			}) {
			continue
		}
		list = append(list, c)
	}
	writeJSON(w, http.StatusOK, containerSummaries(list))
}

func containerSummaries(list []container.Snapshot) []api.ContainerSummary {
	now := time.Now()
	out := []api.ContainerSummary{}
	for _, c := range list {
		labels := c.Config.Labels
		if labels == nil {
			labels = map[string]string{}
		}
		cs := api.ContainerSummary{ID: c.ID, Names: []string{"/" + c.Name}, Image: c.Config.Image, ImageID: c.Config.ImageID,
			Command: strings.Join(c.Command(), " "), Created: c.Created.Unix(), Ports: summaryPorts(c), SizeRw: c.SizeRw,
			Labels: labels, State: string(c.State), Status: humanStatus(c, now)}
		cs.HostConfig.NetworkMode = networkOf(c.Config)
		out = append(out, cs)
	}
	return out
}

// summaryPorts is a summary's Ports, as Docker lists them: each binding
// while the container runs, each exposed port without one.
func summaryPorts(c container.Snapshot) []any {
	out := []any{}
	live := c.Ports
	for _, k := range c.Config.Ports.Keys() {
		bs := live[k]
		if len(bs) == 0 {
			out = append(out, api.Port{PrivatePort: k.Port, Type: k.Proto})
			continue
		}
		for _, b := range bs {
			out = append(out, api.Port{IP: b.Addr().String(), PrivatePort: k.Port, PublicPort: b.HostPort, Type: k.Proto})
		}
	}
	return out
}

func (s *server) containersStart(w http.ResponseWriter, r *http.Request) {
	err := s.d.Containers().Start(context.WithoutCancel(r.Context()), r.PathValue("id"))
	switch {
	case errors.Is(err, container.ErrAlreadyStarted):
		w.WriteHeader(http.StatusNotModified)
	case err != nil:
		writeError(w, statusOf(err), err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *server) containersStop(w http.ResponseWriter, r *http.Request) {
	err := s.d.Containers().Stop(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, container.ErrNotRunning):
		w.WriteHeader(http.StatusNotModified)
	case err != nil:
		writeError(w, statusOf(err), err)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *server) containersRestart(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Containers().Restart(context.WithoutCancel(r.Context()), r.PathValue("id")); err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// containersAttach streams the container's output from now (with logs=1,
// from the start of its log) until its next exit, multiplexed as logs are.
// Docker hijacks the connection for attach; this engine has no stdin to
// carry, so it answers an ordinary streamed response (which also works
// over the in-process transport).
// containersAttach is POST /containers/{id}/attach. As for exec start,
// the connection is not hijacked: the response streams the output
// (multiplexed, or raw for a terminal) and, with stdin=1, the request
// body is the container's input until the client closes it. The stream
// ends when the container stops, the client hangs up, or the input
// carries the detach keys (detachKeys, default ctrl-p,ctrl-q).
func (s *server) containersAttach(w http.ResponseWriter, r *http.Request) {
	stdin, stdout, stderr := boolParam(r, "stdin"), boolParam(r, "stdout"), boolParam(r, "stderr")
	if !boolParam(r, "stream") || (!stdin && !stdout && !stderr) {
		writeError(w, http.StatusBadRequest, errors.New("bad parameters: attach needs stream=1 and one of stdin, stdout, stderr"))
		return
	}
	keys, err := container.ParseDetachKeys(r.URL.Query().Get("detachKeys"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	c, err := s.d.Containers().Get(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	tty := c.Snapshot().Config.Tty
	if stdin && !c.Snapshot().Config.OpenStdin {
		stdin = false // Docker ignores stdin on a container without OpenStdin
	}
	ctype := "application/vnd.docker.multiplexed-stream"
	if tty {
		ctype = "application/vnd.docker.raw-stream"
	}
	rc := http.NewResponseController(w)
	rc.EnableFullDuplex() // read stdin while writing output (not on every transport)
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(http.StatusOK)
	fw := newFlushWriter(w)
	fw.rc.Flush()
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if stdin {
		go func() {
			if errors.Is(c.AttachStdin(ctx, r.Body, keys), container.ErrDetached) {
				cancel() // ends the output stream: the client detached
			}
		}()
	}
	var mu sync.Mutex
	c.Attach(ctx, boolParam(r, "logs"), func(rec container.Record) error {
		if (rec.Stream == container.Stdout && !stdout) || (rec.Stream == container.Stderr && !stderr) {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		if tty {
			_, err := fw.Write(rec.Data)
			return err
		}
		return api.WriteFrame(fw, rec.Stream, rec.Data)
	})
}

// containersResize is POST /containers/{id}/resize?h=&w=: the terminal of
// a running container (run -t).
func (s *server) containersResize(w http.ResponseWriter, r *http.Request) {
	h, err1 := strconv.ParseUint(r.URL.Query().Get("h"), 10, 16)
	wd, err2 := strconv.ParseUint(r.URL.Query().Get("w"), 10, 16)
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, errors.New("bad parameters: resize needs h and w"))
		return
	}
	c, err := s.d.Containers().Get(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	if err := c.Resize(uint16(h), uint16(wd)); err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) containersKill(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Containers().Kill(r.PathValue("id")); err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) containersWait(w http.ResponseWriter, r *http.Request) {
	cond := container.WaitCondition(r.URL.Query().Get("condition"))
	switch cond {
	case "", container.WaitNotRunning, container.WaitNextExit, container.WaitRemoved:
	default:
		writeError(w, http.StatusBadRequest, fmt.Errorf("invalid condition %q", cond))
		return
	}
	c, err := s.d.Containers().Get(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	// Headers go out at once, so a client knows its wait is registered
	// before it starts the container (this is how `wctr run` avoids
	// missing a fast exit); the body follows when the condition holds.
	ready := make(chan struct{})
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		close(ready)
		code, err := c.Wait(r.Context(), cond)
		done <- result{code, err}
	}()
	<-ready
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	http.NewResponseController(w).Flush()
	res := <-done
	resp := api.ContainerWaitResponse{StatusCode: int64(res.code)}
	if res.err != nil {
		resp.Error = &api.ContainerWaitExitError{Message: res.err.Error()}
	} else if s := c.Snapshot(); s.Error != "" && cond != container.WaitRemoved {
		resp.Error = &api.ContainerWaitExitError{Message: s.Error}
	}
	json.NewEncoder(w).Encode(resp)
}

func (s *server) containersLogs(w http.ResponseWriter, r *http.Request) {
	stdout, stderr := boolParam(r, "stdout"), boolParam(r, "stderr")
	if !stdout && !stderr {
		writeError(w, http.StatusBadRequest, errors.New("bad parameters: you must choose at least one stream"))
		return
	}
	c, err := s.d.Containers().Get(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	w.Header().Set("Content-Type", "application/vnd.docker.multiplexed-stream")
	w.WriteHeader(http.StatusOK)
	fw := newFlushWriter(w)
	fw.rc.Flush()
	c.Logs(r.Context(), boolParam(r, "follow"), func(rec container.Record) error {
		if (rec.Stream == container.Stdout && !stdout) || (rec.Stream == container.Stderr && !stderr) {
			return nil
		}
		return api.WriteFrame(fw, rec.Stream, rec.Data)
	})
}

func rfc3339(t time.Time) string {
	if t.IsZero() {
		return "0001-01-01T00:00:00Z"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// inspectPorts fills what inspect says of ports: the configured bindings
// (HostConfig.PortBindings), the exposed ports (Config.ExposedPorts), and
// what is bound while the container runs (NetworkSettings.Ports, every
// exposed port with null for one not published; empty when not running).
func inspectPorts(out *api.ContainerInspect, sn container.Snapshot) {
	out.HostConfig.PublishAllPorts = sn.Config.PublishAll
	out.NetworkSettings.Ports = portmap.APIMap{}
	for _, k := range sn.Config.Ports.Keys() {
		if out.Config.ExposedPorts == nil {
			out.Config.ExposedPorts = map[string]struct{}{}
		}
		out.Config.ExposedPorts[k.String()] = struct{}{}
		if bs := sn.Config.Ports[k]; len(bs) > 0 {
			if out.HostConfig.PortBindings == nil {
				out.HostConfig.PortBindings = portmap.APIMap{}
			}
			out.HostConfig.PortBindings[k.String()] = portmap.Map{k: bs}.API()[k.String()]
		}
	}
	if sn.State == container.Running {
		for k, bs := range sn.Ports {
			out.NetworkSettings.Ports[k.String()] = portmap.Map{k: bs}.API()[k.String()]
		}
	}
}

// restartName is inspect's RestartPolicy.Name: "no" for none, as Docker's.
func restartName(n string) string {
	if n == "" {
		return "no"
	}
	return n
}

func (s *server) containersInspect(w http.ResponseWriter, r *http.Request) {
	c, err := s.d.Containers().Get(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	sn := c.Snapshot()
	argv := sn.Command()
	out := api.ContainerInspect{
		ID: sn.ID, Created: rfc3339(sn.Created), Path: sn.Path, Image: sn.Config.ImageID, Name: "/" + sn.Name,
		Driver: "rootfs (in-memory, copy-on-write)", Platform: "wasip1", SizeRw: sn.SizeRw,
		State: api.ContainerState{Status: string(sn.State), Running: sn.State == container.Running, ExitCode: sn.ExitCode,
			Restarting: sn.State == container.Restarting,
			Error:      sn.Error, StartedAt: rfc3339(sn.StartedAt), FinishedAt: rfc3339(sn.FinishedAt),
			Health: healthState(sn.Health)},
		RestartCount: sn.RestartCount,
		Config: api.ContainerConfig{Image: sn.Config.Image, Cmd: sn.Config.Cmd, Entrypoint: sn.Config.Entrypoint,
			Env: sn.Config.Env, WorkingDir: sn.Config.WorkingDir, Labels: sn.Config.Labels,
			Tty: sn.Config.Tty, OpenStdin: sn.Config.OpenStdin, StdinOnce: sn.Config.StdinOnce, AttachStdin: sn.Config.OpenStdin,
			Healthcheck: healthToAPI(sn.Config.Healthcheck)},
		HostConfig: api.HostConfig{Memory: sn.Config.MemoryLimitBytes, NetworkMode: networkOf(sn.Config),
			RestartPolicy: &api.RestartPolicy{Name: restartName(sn.Config.Restart.Name), MaximumRetryCount: sn.Config.Restart.MaximumRetryCount}},
		Mounts:          []api.MountPoint{},
		NetworkSettings: api.NetworkSettings{Networks: map[string]api.EndpointSettings{}},
	}
	out.NetworkSettings.Networks = s.endpointSettings(sn)
	inspectPorts(&out, sn)
	if sn.IPAddress.IsValid() {
		out.NetworkSettings.IPAddress = sn.IPAddress.String()
	}
	for _, mt := range sn.Config.Mounts {
		mode, rw := "rw", true
		if mt.ReadOnly {
			mode, rw = "ro", false
		}
		out.Mounts = append(out.Mounts, api.MountPoint{Type: "volume", Name: mt.Volume, Source: mt.Volume, Destination: mt.Target, Driver: "local", Mode: mode, RW: rw})
		out.HostConfig.Binds = append(out.HostConfig.Binds, mt.Volume+":"+mt.Target+":"+mode)
	}
	if out.Path == "" && len(argv) > 0 {
		out.Path = argv[0]
	}
	if len(argv) > 1 {
		out.Args = argv[1:]
	} else {
		out.Args = []string{}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) containersDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.d.Containers().Remove(r.Context(), r.PathValue("id"), boolParam(r, "force")); err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
