package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/container"
)

// The exec endpoints; see api/exec.go for how start carries stdin.

func (s *server) containersExec(w http.ResponseWriter, r *http.Request) {
	var req api.ExecCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad exec configuration: %w", err))
		return
	}
	if req.Privileged {
		writeError(w, http.StatusBadRequest, errors.New("privileged exec is not supported"))
		return
	}
	cfg := container.ExecConfig{
		Cmd: req.Cmd, Env: req.Env, WorkingDir: req.WorkingDir, User: req.User, Tty: req.Tty,
		AttachStdin: req.AttachStdin, AttachStdout: req.AttachStdout, AttachStderr: req.AttachStderr,
	}
	if len(req.ConsoleSize) == 2 {
		cfg.ConsoleSize = [2]uint16{req.ConsoleSize[0], req.ConsoleSize[1]}
	}
	x, err := s.d.Containers().CreateExec(r.PathValue("id"), cfg)
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	writeJSON(w, http.StatusCreated, api.IDResponse{ID: x.ID})
}

// frames writes the multiplexed stream: one frame per Write, whole frames
// only (the process's stdout and stderr write from different goroutines).
type frames struct {
	mu *sync.Mutex
	w  io.Writer
	st byte
}

func (f frames) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := api.WriteFrame(f.w, f.st, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (s *server) execStart(w http.ResponseWriter, r *http.Request) {
	dec := json.NewDecoder(r.Body)
	var req api.ExecStartRequest
	if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad start configuration: %w", err))
		return
	}
	m := s.d.Containers()
	x, err := m.GetExec(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	if req.Detach {
		if err := m.StartExec(context.WithoutCancel(r.Context()), x, container.ExecIO{}); err != nil {
			writeError(w, statusOf(err), err)
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}
	cfg := x.Info().Config
	if len(req.ConsoleSize) == 2 && cfg.Tty {
		// The start's size wins over the create's, as in Docker.
		defer x.Resize(req.ConsoleSize[0], req.ConsoleSize[1])
	}
	rc := http.NewResponseController(w)
	rc.EnableFullDuplex() // read stdin while writing output (not on every transport)
	fw := newFlushWriter(w)
	var mu sync.Mutex
	sio := container.ExecIO{Stdout: frames{&mu, fw, container.Stdout}, Stderr: frames{&mu, fw, container.Stderr}}
	ctype := "application/vnd.docker.multiplexed-stream"
	if cfg.Tty {
		sio.Stdout = lockedWriter{&mu, fw}
		ctype = "application/vnd.docker.raw-stream"
	}
	if cfg.AttachStdin {
		sio.Stdin = io.MultiReader(dec.Buffered(), r.Body)
	}
	// The process lives as long as the request: a client that hangs up
	// kills it.
	if err := m.StartExec(r.Context(), x, sio); err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.WriteHeader(http.StatusOK)
	fw.rc.Flush()
	select {
	case <-x.Done():
	case <-r.Context().Done():
		x.Kill()
		<-x.Done()
	}
}

type lockedWriter struct {
	mu *sync.Mutex
	w  io.Writer
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (s *server) execResize(w http.ResponseWriter, r *http.Request) {
	h, err1 := strconv.ParseUint(r.URL.Query().Get("h"), 10, 16)
	wd, err2 := strconv.ParseUint(r.URL.Query().Get("w"), 10, 16)
	if err1 != nil || err2 != nil {
		writeError(w, http.StatusBadRequest, errors.New("bad parameters: resize needs h and w"))
		return
	}
	x, err := s.d.Containers().GetExec(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	if err := x.Resize(uint16(h), uint16(wd)); err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *server) execInspect(w http.ResponseWriter, r *http.Request) {
	x, err := s.d.Containers().GetExec(r.PathValue("id"))
	if err != nil {
		writeError(w, statusOf(err), err)
		return
	}
	in := x.Info()
	out := api.ExecInspect{
		ID: in.ID, ContainerID: in.ContainerID, Running: in.Running, Pid: int(in.Pid),
		OpenStdin: in.Config.AttachStdin, OpenStdout: in.Config.AttachStdout, OpenStderr: in.Config.AttachStderr,
		ProcessConfig: api.ProcessConfig{User: in.Config.User, Tty: in.Config.Tty,
			Entrypoint: in.Config.Cmd[0], Arguments: append([]string{}, in.Config.Cmd[1:]...)},
	}
	if in.Ended {
		code := in.ExitCode
		out.ExitCode = &code
	}
	writeJSON(w, http.StatusOK, out)
}
