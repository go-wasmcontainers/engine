// Package api is the engine's API, compatible with a subset of the Docker
// Engine API (v1.47): what wctrd serves on a unix socket, and the client
// wctr speaks it with. Shapes follow the v1.47 OpenAPI document
// (see docs/REFERENCES.md); only the fields the engine fills are present.
//
// The client side (types, Client, stdcopy) imports neither the daemon nor
// wazero, so the wctr command builds for wasip1.
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
	// Binds are "VOLUME:/path[:ro|:rw]"; named volumes only.
	Binds []string `json:"Binds,omitempty"`
	// NetworkMode is the network to join ("default", "none", a name).
	NetworkMode string `json:"NetworkMode,omitempty"`
	// DeviceRequests asks for devices (run --gpu: one request with the
	// capability "gpu").
	DeviceRequests []DeviceRequest `json:"DeviceRequests,omitempty"`
}

// DeviceRequest is Docker's request for devices from a device driver.
// Capabilities is an OR list of AND lists ([["gpu"]]).
type DeviceRequest struct {
	Driver       string            `json:"Driver,omitempty"`
	Count        int               `json:"Count,omitempty"`
	DeviceIDs    []string          `json:"DeviceIDs,omitempty"`
	Capabilities [][]string        `json:"Capabilities,omitempty"`
	Options      map[string]string `json:"Options,omitempty"`
}

// Network is Docker's network object (GET /networks), a subset.
type Network struct {
	Name       string                     `json:"Name"`
	ID         string                     `json:"Id"`
	Created    string                     `json:"Created"`
	Scope      string                     `json:"Scope"`
	Driver     string                     `json:"Driver"`
	EnableIPv6 bool                       `json:"EnableIPv6"`
	IPAM       NetworkIPAM                `json:"IPAM"`
	Internal   bool                       `json:"Internal"`
	Attachable bool                       `json:"Attachable"`
	Containers map[string]NetworkEndpoint `json:"Containers"`
	Options    map[string]string          `json:"Options"`
	Labels     map[string]string          `json:"Labels"`
}

// NetworkIPAM is a network's address management.
type NetworkIPAM struct {
	Driver string              `json:"Driver"`
	Config []map[string]string `json:"Config"`
}

// NetworkEndpoint is a container on a network.
type NetworkEndpoint struct {
	Name        string `json:"Name"`
	IPv4Address string `json:"IPv4Address"`
}

// NetworkCreateRequest is the body of POST /networks/create.
type NetworkCreateRequest struct {
	Name     string            `json:"Name"`
	Driver   string            `json:"Driver,omitempty"`
	Internal bool              `json:"Internal,omitempty"`
	Labels   map[string]string `json:"Labels,omitempty"`
	Options  map[string]string `json:"Options,omitempty"`
}

// NetworkCreateResponse is the reply to POST /networks/create.
type NetworkCreateResponse struct {
	ID      string `json:"Id"`
	Warning string `json:"Warning"`
}

// MountPoint is one entry of an inspected container's Mounts.
type MountPoint struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	Driver      string `json:"Driver"`
	Mode        string `json:"Mode"`
	RW          bool   `json:"RW"`
}

// Volume is Docker's Volume object.
type Volume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	CreatedAt  string            `json:"CreatedAt"`
	Labels     map[string]string `json:"Labels"`
	Scope      string            `json:"Scope"`
	Options    map[string]string `json:"Options"`
	UsageData  *VolumeUsage      `json:"UsageData,omitempty"`
}

// VolumeUsage is a volume's size and how many containers reference it
// (GET /system/df).
type VolumeUsage struct {
	Size     int64 `json:"Size"`
	RefCount int64 `json:"RefCount"`
}

// VolumeCreateOptions is the body of POST /volumes/create.
type VolumeCreateOptions struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver,omitempty"`
	DriverOpts map[string]string `json:"DriverOpts,omitempty"`
	Labels     map[string]string `json:"Labels,omitempty"`
}

// VolumeListResponse is GET /volumes.
type VolumeListResponse struct {
	Volumes  []Volume `json:"Volumes"`
	Warnings []string `json:"Warnings"`
}

// VolumePruneResponse is POST /volumes/prune.
type VolumePruneResponse struct {
	VolumesDeleted []string `json:"VolumesDeleted"`
	SpaceReclaimed int64    `json:"SpaceReclaimed"`
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
	Mounts       []MountPoint    `json:"Mounts"`
	// NetworkSettings has the address while the container runs.
	NetworkSettings NetworkSettings `json:"NetworkSettings"`
}

// NetworkSettings is an inspected container's networking (a subset).
type NetworkSettings struct {
	IPAddress string                      `json:"IPAddress"`
	Networks  map[string]EndpointSettings `json:"Networks"`
}

// EndpointSettings is a container's place on one network.
type EndpointSettings struct {
	IPAddress string   `json:"IPAddress"`
	DNSNames  []string `json:"DNSNames"`
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

// EventActor is what emitted an event.
type EventActor struct {
	ID         string            `json:"ID"`
	Attributes map[string]string `json:"Attributes"`
}

// EventMessage is one line of GET /events.
type EventMessage struct {
	Type     string     `json:"Type"`
	Action   string     `json:"Action"`
	Actor    EventActor `json:"Actor"`
	Scope    string     `json:"scope"`
	Time     int64      `json:"time"`
	TimeNano int64      `json:"timeNano"`
}

// ContainerStats is one reading of GET /containers/{id}/stats, in
// Docker's shape: memory_stats usage/limit, blkio writes, pids. CPU fields
// are present and zero: a wasm guest gives the engine no CPU accounting,
// which Wctr.CPU says rather than leaving it to be read as idle.
type ContainerStats struct {
	Read        string        `json:"read"`
	Preread     string        `json:"preread"`
	Name        string        `json:"name"`
	ID          string        `json:"id"`
	PidsStats   PidsStats     `json:"pids_stats"`
	MemoryStats MemoryStats   `json:"memory_stats"`
	CPUStats    CPUStats      `json:"cpu_stats"`
	PreCPUStats CPUStats      `json:"precpu_stats"`
	BlkioStats  BlkioStats    `json:"blkio_stats"`
	Wctr        WctrStatsInfo `json:"wctr"`
}

// PidsStats is the process count: 1 while running, else 0.
type PidsStats struct {
	Current uint64 `json:"current"`
}

// MemoryStats is the guest's linear memory and its limit.
type MemoryStats struct {
	Usage uint64 `json:"usage"`
	Limit uint64 `json:"limit"`
}

// CPUStats is Docker's CPU block, left at zero here.
type CPUStats struct {
	CPUUsage struct {
		TotalUsage uint64 `json:"total_usage"`
	} `json:"cpu_usage"`
	SystemCPUUsage uint64 `json:"system_cpu_usage"`
	OnlineCPUs     uint32 `json:"online_cpus"`
}

// BlkioStats carries the bytes written to the container's filesystem.
type BlkioStats struct {
	IOServiceBytesRecursive []BlkioEntry `json:"io_service_bytes_recursive"`
}

// BlkioEntry is one blkio counter.
type BlkioEntry struct {
	Major uint64 `json:"major"`
	Minor uint64 `json:"minor"`
	Op    string `json:"op"`
	Value uint64 `json:"value"`
}

// WctrStatsInfo is this engine's addition to a stats reading.
type WctrStatsInfo struct {
	Running  bool  `json:"running"`
	SizeRw   int64 `json:"size_rw"`
	LogBytes int64 `json:"log_bytes"`
	// CPU says why the CPU fields are zero.
	CPU string `json:"cpu"`
}

// SystemDataUsage is GET /system/df.
type SystemDataUsage struct {
	LayersSize int64              `json:"LayersSize"`
	Images     []ImageSummary     `json:"Images"`
	Containers []ContainerSummary `json:"Containers"`
	Volumes    []Volume           `json:"Volumes"`
	BuildCache []any              `json:"BuildCache"`
	// Storage is this engine's own addition: where its state lives and
	// how full that is. Docker clients ignore it.
	Storage *StorageUsage `json:"Storage,omitempty"`
}

// StorageUsage describes the engine's storage (see SystemDataUsage).
type StorageUsage struct {
	// Kind names the store: "directory /var/lib/wctr", "IndexedDB".
	Kind string `json:"Kind"`
	// Usage and Quota are bytes (-1: unknown); a browser reports its
	// estimate for the whole origin.
	Usage int64 `json:"Usage"`
	Quota int64 `json:"Quota"`
	// Persistent says the browser will not evict it under pressure.
	Persistent bool `json:"Persistent"`
	// Error is the last failure to store the engine's state, if any.
	Error string `json:"Error,omitempty"`
}

// FilesystemChange is one entry of GET /containers/{id}/changes; Kind is
// 0 modified, 1 added, 2 deleted.
type FilesystemChange struct {
	Path string `json:"Path"`
	Kind int    `json:"Kind"`
}

// ContainerPruneResponse is POST /containers/prune.
type ContainerPruneResponse struct {
	ContainersDeleted []string `json:"ContainersDeleted"`
	SpaceReclaimed    int64    `json:"SpaceReclaimed"`
}

// ImagePruneResponse is POST /images/prune.
type ImagePruneResponse struct {
	ImagesDeleted  []ImageDeleteResponseItem `json:"ImagesDeleted"`
	SpaceReclaimed int64                     `json:"SpaceReclaimed"`
}
