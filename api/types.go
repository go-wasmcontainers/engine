// Package api is the engine's subset of the Docker Engine API (v1.47):
// the HTTP server dockerd serves on a unix socket, and the client the
// docker command speaks it with. Shapes follow the v1.47 OpenAPI document
// (see docs/REFERENCES.md); only the fields the engine fills are present.
//
// The client side (types, Client, stdcopy) imports neither the daemon nor
// wazero, so the docker command builds for wasip1.
package api

import (
	"encoding/json"
	"errors"
)

// APIVersion is the version the server speaks; MinAPIVersion the oldest
// it accepts in a /vX.Y prefix.
const (
	APIVersion    = "1.47"
	MinAPIVersion = "1.24"
)

// ErrorResponse is the body of every error.
type ErrorResponse struct {
	Message string `json:"message"`
}

// SystemVersion is GET /version.
type SystemVersion struct {
	Platform      struct{ Name string } `json:"Platform"`
	Version       string                `json:"Version"`
	APIVersion    string                `json:"ApiVersion"`
	MinAPIVersion string                `json:"MinAPIVersion"`
	GitCommit     string                `json:"GitCommit"`
	GoVersion     string                `json:"GoVersion"`
	Os            string                `json:"Os"`
	Arch          string                `json:"Arch"`
	KernelVersion string                `json:"KernelVersion"`
	BuildTime     string                `json:"BuildTime,omitempty"`
}

// ImageSummary is one entry of GET /images/json.
type ImageSummary struct {
	ID          string            `json:"Id"`
	ParentID    string            `json:"ParentId"`
	RepoTags    []string          `json:"RepoTags"`
	RepoDigests []string          `json:"RepoDigests"`
	Created     int64             `json:"Created"`
	Size        int64             `json:"Size"`
	SharedSize  int64             `json:"SharedSize"`
	Labels      map[string]string `json:"Labels"`
	Containers  int64             `json:"Containers"`
}

// ImageInspect is GET /images/{name}/json (a subset).
type ImageInspect struct {
	ID           string           `json:"Id"`
	RepoTags     []string         `json:"RepoTags"`
	RepoDigests  []string         `json:"RepoDigests"`
	Created      string           `json:"Created"`
	Author       string           `json:"Author"`
	Architecture string           `json:"Architecture"`
	Os           string           `json:"Os"`
	Size         int64            `json:"Size"`
	Config       ContainerConfig  `json:"Config"`
	RootFS       map[string]any   `json:"RootFS"`
	Metadata     map[string]any   `json:"Metadata,omitempty"`
	Descriptor   *json.RawMessage `json:"Descriptor,omitempty"`
}

// ImageDeleteResponseItem is one entry of DELETE /images/{name}.
type ImageDeleteResponseItem struct {
	Untagged string `json:"Untagged,omitempty"`
	Deleted  string `json:"Deleted,omitempty"`
}

// StrSlice accepts a JSON string or array of strings, as Docker's does
// for Cmd and Entrypoint; null stays nil ("use the image's").
type StrSlice []string

// UnmarshalJSON implements json.Unmarshaler.
func (s *StrSlice) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*s = nil
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err == nil {
		*s = StrSlice{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err != nil {
		return errors.New("must be a string or an array of strings")
	}
	if many == nil {
		many = []string{}
	}
	*s = many
	return nil
}

// ContainerConfig is the create body's container part.
type ContainerConfig struct {
	Image      string            `json:"Image"`
	Cmd        StrSlice          `json:"Cmd"`
	Entrypoint StrSlice          `json:"Entrypoint"`
	Env        []string          `json:"Env"`
	WorkingDir string            `json:"WorkingDir"`
	Labels     map[string]string `json:"Labels"`
	Tty        bool              `json:"Tty"`
}

// HostConfig is the create body's host part (a subset).
type HostConfig struct {
	Memory     int64 `json:"Memory"`
	AutoRemove bool  `json:"AutoRemove"`
}

// ContainerCreateRequest is the body of POST /containers/create.
type ContainerCreateRequest struct {
	ContainerConfig
	HostConfig HostConfig `json:"HostConfig"`
}

// ContainerCreateResponse is the reply to POST /containers/create.
type ContainerCreateResponse struct {
	ID       string   `json:"Id"`
	Warnings []string `json:"Warnings"`
}

// ContainerWaitExitError is the error part of a wait reply.
type ContainerWaitExitError struct {
	Message string `json:"Message"`
}

// ContainerWaitResponse is the reply to POST /containers/{id}/wait.
type ContainerWaitResponse struct {
	StatusCode int64                   `json:"StatusCode"`
	Error      *ContainerWaitExitError `json:"Error"`
}

// ContainerSummary is one entry of GET /containers/json.
type ContainerSummary struct {
	ID         string            `json:"Id"`
	Names      []string          `json:"Names"`
	Image      string            `json:"Image"`
	ImageID    string            `json:"ImageID"`
	Command    string            `json:"Command"`
	Created    int64             `json:"Created"`
	Ports      []any             `json:"Ports"`
	SizeRw     int64             `json:"SizeRw,omitempty"`
	Labels     map[string]string `json:"Labels"`
	State      string            `json:"State"`
	Status     string            `json:"Status"`
	HostConfig struct {
		NetworkMode string `json:"NetworkMode"`
	} `json:"HostConfig"`
}

// ContainerState is the State of an inspect reply.
type ContainerState struct {
	Status     string `json:"Status"`
	Running    bool   `json:"Running"`
	Paused     bool   `json:"Paused"`
	Restarting bool   `json:"Restarting"`
	OOMKilled  bool   `json:"OOMKilled"`
	Dead       bool   `json:"Dead"`
	Pid        int    `json:"Pid"`
	ExitCode   int    `json:"ExitCode"`
	Error      string `json:"Error"`
	StartedAt  string `json:"StartedAt"`
	FinishedAt string `json:"FinishedAt"`
}

// ContainerInspect is GET /containers/{id}/json (a subset).
type ContainerInspect struct {
	ID           string          `json:"Id"`
	Created      string          `json:"Created"`
	Path         string          `json:"Path"`
	Args         []string        `json:"Args"`
	State        ContainerState  `json:"State"`
	Image        string          `json:"Image"`
	Name         string          `json:"Name"`
	RestartCount int             `json:"RestartCount"`
	Driver       string          `json:"Driver"`
	Platform     string          `json:"Platform"`
	SizeRw       int64           `json:"SizeRw"`
	Config       ContainerConfig `json:"Config"`
	HostConfig   HostConfig      `json:"HostConfig"`
}

// JSONMessage is one line of the POST /images/create progress stream.
type JSONMessage struct {
	Status      string        `json:"status,omitempty"`
	ID          string        `json:"id,omitempty"`
	Error       string        `json:"error,omitempty"`
	ErrorDetail *JSONMsgError `json:"errorDetail,omitempty"`
}

// JSONMsgError is the error detail of a JSONMessage.
type JSONMsgError struct {
	Message string `json:"message"`
}
