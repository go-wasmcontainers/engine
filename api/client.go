package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Client speaks the API to a daemon.
type Client struct {
	http *http.Client
	base string
}

// ErrDaemon is wrapped by every error reply of the daemon.
type ErrDaemon struct {
	Status  int
	Message string
}

func (e *ErrDaemon) Error() string { return "Error response from daemon: " + e.Message }

// IsNotFound reports a 404 from the daemon.
func IsNotFound(err error) bool {
	var e *ErrDaemon
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

// NewClient connects to host: "unix:///path/to/socket".
func NewClient(host string) (*Client, error) {
	u, err := url.Parse(host)
	if err != nil || u.Scheme != "unix" || u.Path == "" {
		return nil, fmt.Errorf("unsupported daemon host %q: only unix:///path is supported", host)
	}
	sock := u.Path
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		DisableCompression: true,
	}
	return &Client{http: &http.Client{Transport: tr}, base: "http://wctr/v" + APIVersion}, nil
}

// NewClientWith uses a caller-provided HTTP client and base URL (tests,
// and later the browser's own transport).
func NewClientWith(hc *http.Client, base string) *Client {
	return &Client{http: hc, base: strings.TrimSuffix(base, "/") + "/v" + APIVersion}
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cannot connect to the daemon: %w", err)
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e ErrorResponse
		if json.Unmarshal(b, &e) != nil || e.Message == "" {
			e.Message = strings.TrimSpace(string(b))
			if e.Message == "" {
				e.Message = resp.Status
			}
		}
		return nil, &ErrDaemon{Status: resp.StatusCode, Message: e.Message}
	}
	return resp, nil
}

func (c *Client) getJSON(ctx context.Context, method, path string, q url.Values, body, out any) error {
	resp, err := c.do(ctx, method, path, q, body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(out)
}

// Ping checks the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.getJSON(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// Version returns the daemon's version.
func (c *Client) Version(ctx context.Context) (SystemVersion, error) {
	var v SystemVersion
	err := c.getJSON(ctx, http.MethodGet, "/version", nil, nil, &v)
	return v, err
}

// Pull pulls ref, calling progress for each message; an error message in
// the stream is returned as an error.
func (c *Client) Pull(ctx context.Context, ref string, progress func(JSONMessage)) error {
	resp, err := c.do(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {ref}}, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m JSONMessage
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return fmt.Errorf("bad progress message: %w", err)
		}
		if m.Error != "" {
			return &ErrDaemon{Status: http.StatusOK, Message: m.Error}
		}
		if progress != nil {
			progress(m)
		}
	}
	return sc.Err()
}

// Images lists images.
func (c *Client) Images(ctx context.Context) ([]ImageSummary, error) {
	var out []ImageSummary
	err := c.getJSON(ctx, http.MethodGet, "/images/json", nil, nil, &out)
	return out, err
}

// imagePath escapes an image reference segment by segment: it may hold
// slashes (repository paths) but nothing that changes the URL's shape.
func imagePath(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/images/" + strings.Join(parts, "/")
}

// ImageInspect inspects an image.
func (c *Client) ImageInspect(ctx context.Context, ref string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.getJSON(ctx, http.MethodGet, imagePath(ref)+"/json", nil, nil, &out)
	return out, err
}

// RemoveImage removes an image.
func (c *Client) RemoveImage(ctx context.Context, ref string, force bool) ([]ImageDeleteResponseItem, error) {
	var out []ImageDeleteResponseItem
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	err := c.getJSON(ctx, http.MethodDelete, imagePath(ref), q, nil, &out)
	return out, err
}

// Create creates a container.
func (c *Client) Create(ctx context.Context, name string, req ContainerCreateRequest) (ContainerCreateResponse, error) {
	var out ContainerCreateResponse
	q := url.Values{}
	if name != "" {
		q.Set("name", name)
	}
	err := c.getJSON(ctx, http.MethodPost, "/containers/create", q, req, &out)
	return out, err
}

// Start starts a container.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.getJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil)
}

// Stop stops a running container and returns once it has exited; a
// container that is not running is not an error (the daemon answers 304).
func (c *Client) Stop(ctx context.Context, id string) error {
	return c.getJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop", nil, nil, nil)
}

// Restart stops a container if it runs, then starts it.
func (c *Client) Restart(ctx context.Context, id string) error {
	return c.getJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/restart", nil, nil, nil)
}

// Attach streams a container's output from now until its next exit,
// multiplexed (see Demux). Attach before Start so nothing is missed.
func (c *Client) Attach(ctx context.Context, id string) (io.ReadCloser, error) {
	q := url.Values{"stream": {"1"}, "stdout": {"1"}, "stderr": {"1"}}
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/attach", q, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Kill kills a running container.
func (c *Client) Kill(ctx context.Context, id string) error {
	return c.getJSON(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/kill", nil, nil, nil)
}

// Waiter is a registered wait: the daemon has acknowledged it, and Result
// blocks until the condition holds.
type Waiter struct{ resp *http.Response }

// Result blocks for the wait's answer.
func (w *Waiter) Result() (ContainerWaitResponse, error) {
	defer w.resp.Body.Close()
	var out ContainerWaitResponse
	err := json.NewDecoder(io.LimitReader(w.resp.Body, 1<<20)).Decode(&out)
	return out, err
}

// Wait registers a wait for condition; it returns once the daemon has
// acknowledged it, before the container exits.
func (c *Client) Wait(ctx context.Context, id, condition string) (*Waiter, error) {
	q := url.Values{}
	if condition != "" {
		q.Set("condition", condition)
	}
	resp, err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/wait", q, nil)
	if err != nil {
		return nil, err
	}
	return &Waiter{resp: resp}, nil
}

// Logs returns the multiplexed log stream; demultiplex it with Demux.
func (c *Client) Logs(ctx context.Context, id string, follow bool) (io.ReadCloser, error) {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}}
	if follow {
		q.Set("follow", "1")
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs", q, nil)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// Containers lists containers.
func (c *Client) Containers(ctx context.Context, all bool) ([]ContainerSummary, error) {
	q := url.Values{}
	if all {
		q.Set("all", "1")
	}
	var out []ContainerSummary
	err := c.getJSON(ctx, http.MethodGet, "/containers/json", q, nil, &out)
	return out, err
}

// Inspect inspects a container.
func (c *Client) Inspect(ctx context.Context, id string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.getJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out)
	return out, err
}

// Changes lists what a container changed in its filesystem.
func (c *Client) Changes(ctx context.Context, id string) ([]FilesystemChange, error) {
	var out []FilesystemChange
	err := c.getJSON(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/changes", nil, nil, &out)
	return out, err
}

// EventsOptions selects events: since and until as GET /events takes
// them (Unix seconds, RFC 3339 or a duration back from now), filters as
// key → values.
type EventsOptions struct {
	Since, Until string
	Filters      map[string][]string
}

// Events streams events to fn until the stream ends (until reached, the
// context cancelled, or the daemon cutting off a slow reader).
func (c *Client) Events(ctx context.Context, o EventsOptions, fn func(EventMessage) error) error {
	q := url.Values{}
	if o.Since != "" {
		q.Set("since", o.Since)
	}
	if o.Until != "" {
		q.Set("until", o.Until)
	}
	if len(o.Filters) > 0 {
		b, err := json.Marshal(o.Filters)
		if err != nil {
			return err
		}
		q.Set("filters", string(b))
	}
	resp, err := c.do(ctx, http.MethodGet, "/events", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var m EventMessage
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := fn(m); err != nil {
			return err
		}
	}
}

// VolumeCreate makes a volume (or returns the existing one).
func (c *Client) VolumeCreate(ctx context.Context, o VolumeCreateOptions) (Volume, error) {
	var out Volume
	err := c.getJSON(ctx, http.MethodPost, "/volumes/create", nil, o, &out)
	return out, err
}

// Volumes lists volumes.
func (c *Client) Volumes(ctx context.Context) ([]Volume, error) {
	var out VolumeListResponse
	err := c.getJSON(ctx, http.MethodGet, "/volumes", nil, nil, &out)
	return out.Volumes, err
}

// VolumeInspect inspects a volume.
func (c *Client) VolumeInspect(ctx context.Context, name string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.getJSON(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, nil, &out)
	return out, err
}

// VolumeRemove removes a volume.
func (c *Client) VolumeRemove(ctx context.Context, name string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	return c.getJSON(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), q, nil, nil)
}

// VolumesPrune removes unused anonymous volumes, or with all every unused
// one.
func (c *Client) VolumesPrune(ctx context.Context, all bool) (VolumePruneResponse, error) {
	q := url.Values{}
	if all {
		q.Set("filters", `{"all":["true"]}`)
	}
	var out VolumePruneResponse
	err := c.getJSON(ctx, http.MethodPost, "/volumes/prune", q, nil, &out)
	return out, err
}

// NetworkCreate makes an in-engine network.
func (c *Client) NetworkCreate(ctx context.Context, o NetworkCreateRequest) (NetworkCreateResponse, error) {
	var out NetworkCreateResponse
	err := c.getJSON(ctx, http.MethodPost, "/networks/create", nil, o, &out)
	return out, err
}

// Networks lists networks.
func (c *Client) Networks(ctx context.Context) ([]Network, error) {
	var out []Network
	err := c.getJSON(ctx, http.MethodGet, "/networks", nil, nil, &out)
	return out, err
}

// NetworkInspect inspects a network.
func (c *Client) NetworkInspect(ctx context.Context, ref string) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.getJSON(ctx, http.MethodGet, "/networks/"+url.PathEscape(ref), nil, nil, &out)
	return out, err
}

// NetworkRemove removes a network.
func (c *Client) NetworkRemove(ctx context.Context, ref string) error {
	return c.getJSON(ctx, http.MethodDelete, "/networks/"+url.PathEscape(ref), nil, nil, nil)
}

// DiskUsage is GET /system/df.
func (c *Client) DiskUsage(ctx context.Context) (SystemDataUsage, error) {
	var out SystemDataUsage
	err := c.getJSON(ctx, http.MethodGet, "/system/df", nil, nil, &out)
	return out, err
}

// PruneContainers removes every stopped container.
func (c *Client) PruneContainers(ctx context.Context) (ContainerPruneResponse, error) {
	var out ContainerPruneResponse
	err := c.getJSON(ctx, http.MethodPost, "/containers/prune", nil, nil, &out)
	return out, err
}

// PruneImages removes dangling images, or with all every image no
// container uses.
func (c *Client) PruneImages(ctx context.Context, all bool) (ImagePruneResponse, error) {
	q := url.Values{}
	if all {
		q.Set("filters", `{"dangling":["false"]}`)
	}
	var out ImagePruneResponse
	err := c.getJSON(ctx, http.MethodPost, "/images/prune", q, nil, &out)
	return out, err
}

// Stats reads a container's stats once (stream false) or once a second
// until fn returns an error or the stream ends.
func (c *Client) Stats(ctx context.Context, id string, stream bool, fn func(ContainerStats) error) error {
	q := url.Values{"stream": {"0"}}
	if stream {
		q.Set("stream", "1")
	}
	resp, err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/stats", q, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(io.LimitReader(resp.Body, 1<<40))
	for {
		var s ContainerStats
		if err := dec.Decode(&s); err != nil {
			if errors.Is(err, io.EOF) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		if err := fn(s); err != nil {
			return err
		}
		if !stream {
			return nil
		}
	}
}

// Remove removes a container.
func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	q := url.Values{}
	if force {
		q.Set("force", "1")
	}
	return c.getJSON(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id), q, nil, nil)
}
