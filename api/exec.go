package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// Exec (Docker Engine API v1.47, tag "Exec"): POST /containers/{id}/exec,
// POST /exec/{id}/start, POST /exec/{id}/resize, GET /exec/{id}/json.
//
// One difference: Docker hijacks the connection for /exec/{id}/start. This
// engine answers an ordinary streamed response instead (as for attach),
// which also works over the in-process transport, and reads stdin from the
// request body: the body is the ExecStartConfig JSON object, followed —
// when the instance attaches stdin — by the raw bytes of stdin, until the
// client closes it. The response is the multiplexed stream
// (application/vnd.docker.multiplexed-stream), or the raw stream
// (application/vnd.docker.raw-stream) for a terminal.

// ExecCreateRequest is the body of POST /containers/{id}/exec.
type ExecCreateRequest struct {
	AttachStdin  bool     `json:"AttachStdin,omitempty"`
	AttachStdout bool     `json:"AttachStdout,omitempty"`
	AttachStderr bool     `json:"AttachStderr,omitempty"`
	ConsoleSize  []uint16 `json:"ConsoleSize,omitempty"` // [height, width]
	DetachKeys   string   `json:"DetachKeys,omitempty"`
	Tty          bool     `json:"Tty,omitempty"`
	Env          []string `json:"Env,omitempty"`
	Cmd          []string `json:"Cmd"`
	Privileged   bool     `json:"Privileged,omitempty"`
	User         string   `json:"User,omitempty"`
	WorkingDir   string   `json:"WorkingDir,omitempty"`
}

// IDResponse is the reply to POST /containers/{id}/exec.
type IDResponse struct {
	ID string `json:"Id"`
}

// ExecStartRequest is the body of POST /exec/{id}/start.
type ExecStartRequest struct {
	Detach      bool     `json:"Detach,omitempty"`
	Tty         bool     `json:"Tty,omitempty"`
	ConsoleSize []uint16 `json:"ConsoleSize,omitempty"`
}

// ProcessConfig is an exec instance's command (ExecInspect).
type ProcessConfig struct {
	Privileged bool     `json:"privileged"`
	User       string   `json:"user"`
	Tty        bool     `json:"tty"`
	Entrypoint string   `json:"entrypoint"`
	Arguments  []string `json:"arguments"`
}

// ExecInspect is the reply to GET /exec/{id}/json.
type ExecInspect struct {
	CanRemove     bool          `json:"CanRemove"`
	DetachKeys    string        `json:"DetachKeys"`
	ID            string        `json:"ID"`
	Running       bool          `json:"Running"`
	ExitCode      *int          `json:"ExitCode"`
	ProcessConfig ProcessConfig `json:"ProcessConfig"`
	OpenStdin     bool          `json:"OpenStdin"`
	OpenStderr    bool          `json:"OpenStderr"`
	OpenStdout    bool          `json:"OpenStdout"`
	ContainerID   string        `json:"ContainerID"`
	Pid           int           `json:"Pid"`
}

// ExecCreate prepares a command in a running container.
func (c *Client) ExecCreate(ctx context.Context, container string, req ExecCreateRequest) (string, error) {
	var out IDResponse
	err := c.getJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(container)+"/exec", nil, req, &out)
	return out.ID, err
}

// ExecStart starts an instance. Unless detached, it returns the output
// stream (multiplexed; raw with a terminal), and copies stdin (if not
// nil) into the request after the start object; closing the returned
// body hangs up, which kills the process.
func (c *Client) ExecStart(ctx context.Context, id string, req ExecStartRequest, stdin io.Reader) (io.ReadCloser, error) {
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	var body io.Reader = bytes.NewReader(b)
	if stdin != nil && !req.Detach {
		body = io.MultiReader(body, stdin)
	}
	resp, err := c.doRaw(ctx, http.MethodPost, "/exec/"+url.PathEscape(id)+"/start", nil, body, "application/json")
	if err != nil {
		return nil, err
	}
	if req.Detach {
		resp.Body.Close()
		return nil, nil
	}
	return resp.Body, nil
}

// ExecResize changes the terminal size of a running exec -t.
func (c *Client) ExecResize(ctx context.Context, id string, height, width uint16) error {
	q := url.Values{"h": {strconv.Itoa(int(height))}, "w": {strconv.Itoa(int(width))}}
	return c.getJSON(ctx, http.MethodPost, "/exec/"+url.PathEscape(id)+"/resize", q, nil, nil)
}

// ExecInspect returns an instance's state, its exit code once ended.
func (c *Client) ExecInspect(ctx context.Context, id string) (ExecInspect, error) {
	var out ExecInspect
	err := c.getJSON(ctx, http.MethodGet, "/exec/"+url.PathEscape(id)+"/json", nil, nil, &out)
	return out, err
}
