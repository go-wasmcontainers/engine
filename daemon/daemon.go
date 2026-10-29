// Package daemon wires the engine together: the image store, the root
// filesystems built from it, and the container manager. The API server is
// a thin HTTP face over it.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/events"
	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/network"
	"github.com/go-wasmcontainers/engine/portmap"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/volume"
)

// ErrImageInUse is returned when removing an image a container uses.
var ErrImageInUse = errors.New("image is being used by a container")

// ErrNoSuchImage is returned for an unknown image reference.
var ErrNoSuchImage = errors.New("no such image")

// Options configures a daemon.
type Options struct {
	// Root holds the image store on disk. One of Root and Backend is
	// required.
	Root string
	// Backend holds the image store elsewhere (memory, IndexedDB).
	Backend image.Backend
	// Limits bound pulls; zero means image.DefaultLimits.
	Limits image.Limits
	// Unpack bounds layer unpacking; zero means rootfs.DefaultLimits.
	Unpack rootfs.Limits
	// Registry configures registry pulls.
	Registry image.RegistryOptions
	// Containers configures the container manager (Executor required).
	Containers container.Options
	// MaxLowers bounds how many unpacked image trees stay in memory.
	MaxLowers int
	// StorageInfo, when set, reports the state store's usage for
	// `system df` (a browser's quota estimate, a disk's free space).
	StorageInfo func() StorageInfo
	// ImagesRemoved, when set, is called after images were deleted (the
	// browser executor drops its compiled-module cache then).
	ImagesRemoved func()
	// Volumes configures named volumes (they share Containers.Store).
	Volumes volume.Options
	// Networks configures networks (they share Containers.Store); every
	// engine has "bridge", its default, and "none".
	Networks network.Options
}

// Daemon is one engine instance.
type Daemon struct {
	opt   Options
	store *image.Store
	mgr   *container.Manager

	mu     sync.Mutex
	lowers map[image.Digest]*lowerEntry // by manifest digest
	tick   uint64

	restored container.RestoreReport
	events   *events.Bus
	vols     *volume.Manager
	nets     *network.Manager
}

// Networks is the network manager.
func (d *Daemon) Networks() *network.Manager { return d.nets }

// Volumes is the volume manager.
func (d *Daemon) Volumes() *volume.Manager { return d.vols }

// Events is the engine's event bus (GET /events).
func (d *Daemon) Events() *events.Bus { return d.events }

type lowerEntry struct {
	fs   *rootfs.FS
	used uint64
}

// New opens the store under opt.Root.
func New(opt Options) (*Daemon, error) {
	if opt.Root == "" && opt.Backend == nil {
		return nil, errors.New("daemon: no data root")
	}
	if opt.Containers.Executor == nil {
		return nil, errors.New("daemon: no executor")
	}
	if opt.Limits == (image.Limits{}) {
		opt.Limits = image.DefaultLimits
	}
	if opt.Unpack == (rootfs.Limits{}) {
		opt.Unpack = rootfs.DefaultLimits
	}
	if opt.MaxLowers <= 0 {
		opt.MaxLowers = 16
	}
	var st *image.Store
	var err error
	if opt.Backend != nil {
		st, err = image.NewStore(opt.Backend)
	} else {
		st, err = image.OpenStore(filepath.Join(opt.Root, "images"))
	}
	if err != nil {
		return nil, err
	}
	bus := events.New()
	opt.Containers.Events = bus
	if opt.Volumes.Store == nil {
		opt.Volumes.Store = opt.Containers.Store
	}
	vols := volume.New(opt.Volumes)
	// Volumes first: restored containers mount them.
	vlim := rootfs.Limits{MaxBytes: 1 << 30, MaxFiles: 1 << 17, MaxRatio: 1, MaxPathLen: 4096, MaxDepth: 256}
	repairedVols, err := vols.Restore(vlim)
	if err != nil {
		return nil, fmt.Errorf("daemon: restoring volumes: %w", err)
	}
	opt.Containers.Volume = func(name string) (*rootfs.FS, error) {
		v, err := vols.Get(name)
		if err != nil {
			return nil, err
		}
		return v.FS(), nil
	}
	if opt.Networks.Store == nil {
		opt.Networks.Store = opt.Containers.Store
	}
	nets, repairedNets, err := network.New(opt.Networks)
	if err != nil {
		return nil, fmt.Errorf("daemon: networks: %w", err)
	}
	repairedVols = append(repairedVols, repairedNets...)
	opt.Containers.Network = nets.Attach
	d := &Daemon{opt: opt, store: st, mgr: container.NewManager(opt.Containers), lowers: map[image.Digest]*lowerEntry{}, events: bus, vols: vols, nets: nets}
	// Containers from the last run, if any were stored.
	rep, err := d.mgr.Restore(func(id string) (*rootfs.FS, error) {
		_, img, err := d.ResolveImage(id)
		if err != nil {
			return nil, err
		}
		return d.lower(img)
	}, rootfs.Limits{
		MaxBytes: max(opt.Containers.Quota.MaxBytes, 1<<20) + 1<<20, MaxFiles: max(opt.Containers.Quota.MaxFiles, 1<<10) + 1<<10,
		MaxRatio: 1, MaxPathLen: 4096, MaxDepth: 256,
	})
	if err != nil {
		return nil, fmt.Errorf("daemon: restoring containers: %w", err)
	}
	rep.Repaired = append(repairedVols, rep.Repaired...)
	// Restart policies: always, and unless-stopped when no user stopped it,
	// start with the engine.
	for _, err := range d.mgr.AutoStart(context.Background()) {
		rep.Repaired = append(rep.Repaired, "restart policy: "+err.Error())
	}
	d.restored = rep
	return d, nil
}

// Restored says what was restored from the state store at start.
func (d *Daemon) Restored() container.RestoreReport { return d.restored }

// Containers is the container manager.
func (d *Daemon) Containers() *container.Manager { return d.mgr }

// Close kills running containers.
func (d *Daemon) Close(ctx context.Context) error {
	d.mgr.Shutdown(ctx)
	d.nets.Close()
	return d.vols.Close(ctx)
}

// Pull fetches ref and names it; it returns the local name.
func (d *Daemon) Pull(ctx context.Context, ref string, progress image.Progress) (string, *image.PullResult, error) {
	src, name, err := image.OpenSource(ref, d.opt.Registry, d.opt.Limits)
	if err != nil {
		return "", nil, err
	}
	res, err := image.Pull(ctx, d.store, src, d.opt.Limits, progress)
	if err != nil {
		return "", nil, err
	}
	// Prove the layers unpack before naming the image, so a pull never
	// leaves a name that cannot run.
	if _, err := d.lower(res.Image); err != nil {
		return "", nil, err
	}
	if err := d.store.Tag(name, image.Ref{Manifest: res.Image.ManifestDigest, RepoDigest: res.RepoDigest}); err != nil {
		return "", nil, err
	}
	d.events.Publish(events.Event{Type: events.Image, Action: "pull", ID: name, Attributes: map[string]string{"name": name}})
	return name, res, nil
}

// ImageInfo is one image as `docker images` lists it.
type ImageInfo struct {
	ID          image.Digest
	RepoTags    []string
	RepoDigests []string
	Created     time.Time
	Size        int64
	Labels      map[string]string
	Containers  int
	Image       *image.Image
}

// Images lists images, one entry per image ID, newest first.
func (d *Daemon) Images() ([]ImageInfo, error) {
	byID := map[image.Digest]*ImageInfo{}
	for _, nr := range d.store.Names() {
		img, err := d.store.Image(nr.Manifest, d.opt.Limits)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", nr.Name, err)
		}
		info := byID[img.ID()]
		if info == nil {
			created, _ := time.Parse(time.RFC3339Nano, img.Config.Created)
			info = &ImageInfo{ID: img.ID(), Created: created, Size: img.Size, Labels: img.Config.Config.Labels, Image: img}
			info.Containers = len(d.mgr.UsesImage(string(img.ID())))
			byID[img.ID()] = info
		}
		if image.IsDangling(nr.Name) {
			continue // kept with no name: <none>
		}
		info.RepoTags = append(info.RepoTags, nr.Name)
		if nr.RepoDigest != "" {
			repo := nr.Name
			if i := strings.LastIndexAny(repo, ":@"); i > strings.LastIndexByte(repo, '/') {
				repo = repo[:i]
			}
			rd := repo + "@" + string(nr.RepoDigest)
			if !contains(info.RepoDigests, rd) {
				info.RepoDigests = append(info.RepoDigests, rd)
			}
		}
	}
	out := make([]ImageInfo, 0, len(byID))
	for _, v := range byID {
		out = append(out, *v)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

// ResolveImage finds an image by name ("hello", "hello:v1",
// "localhost:5000/x@sha256:…"), by ID ("sha256:…") or by unique ID prefix.
func (d *Daemon) ResolveImage(ref string) (string, *image.Image, error) {
	if image.IsLayoutRef(ref) {
		// "oci:<dir>:<name>" names what it was pulled as.
		lr, err := image.ParseLayoutRef(ref)
		if err != nil {
			return "", nil, fmt.Errorf("%w: %v", ErrNoSuchImage, err)
		}
		if ref, err = lr.LocalName(); err != nil {
			return "", nil, fmt.Errorf("%w: %v", ErrNoSuchImage, err)
		}
	}
	if name, err := image.NormalizeName(ref); err == nil {
		if r, ok := d.store.Resolve(name); ok {
			img, err := d.store.Image(r.Manifest, d.opt.Limits)
			return name, img, err
		}
	}
	id := strings.TrimPrefix(ref, "sha256:")
	if len(id) == 0 || len(id) > 64 || strings.Trim(id, "0123456789abcdef") != "" {
		return "", nil, fmt.Errorf("%w: %s", ErrNoSuchImage, ref)
	}
	var found *image.Image
	for _, nr := range d.store.Names() {
		img, err := d.store.Image(nr.Manifest, d.opt.Limits)
		if err != nil {
			continue
		}
		if strings.HasPrefix(img.ID().Encoded(), id) {
			if found != nil && found.ID() != img.ID() {
				return "", nil, fmt.Errorf("%w: %s is ambiguous", ErrNoSuchImage, ref)
			}
			found = img
		}
	}
	if found == nil {
		return "", nil, fmt.Errorf("%w: %s", ErrNoSuchImage, ref)
	}
	return "", found, nil
}

// lower returns the frozen root filesystem of an image, unpacking it once.
func (d *Daemon) lower(img *image.Image) (*rootfs.FS, error) {
	d.mu.Lock()
	d.tick++
	if e, ok := d.lowers[img.ManifestDigest]; ok {
		e.used = d.tick
		d.mu.Unlock()
		return e.fs, nil
	}
	d.mu.Unlock()
	layers := make([]rootfs.LayerReader, len(img.Manifest.Layers))
	for i, l := range img.Manifest.Layers {
		l := l
		layers[i] = rootfs.LayerReader{MediaType: l.MediaType, Title: l.Annotations[image.AnnotationTitle],
			Open: func() (io.ReadCloser, error) {
				f, err := d.store.Open(l.Digest)
				if err != nil {
					return nil, err
				}
				// The store verified the blob when it was written; reading it
				// back through a verifier costs one hash and survives disk rot.
				return readCloser{image.NewVerifyingReader(f, l.Digest, l.Size), f}, nil
			}}
	}
	fs, err := rootfs.Build(layers, d.opt.Unpack)
	if err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.lowers) >= d.opt.MaxLowers {
		var oldK image.Digest
		var old *lowerEntry
		for k, e := range d.lowers {
			if old == nil || e.used < old.used {
				oldK, old = k, e
			}
		}
		delete(d.lowers, oldK) // containers keep their own reference
	}
	d.lowers[img.ManifestDigest] = &lowerEntry{fs: fs, used: d.tick}
	return fs, nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

// CreateRequest is a container create, with Docker's override rules: a
// set Entrypoint replaces the image's and drops its Cmd; a set Cmd
// replaces the image's Cmd; Env entries override the image's by key.
type CreateRequest struct {
	Name       string
	Image      string
	Entrypoint []string // nil = image's
	Cmd        []string // nil = image's
	Env        []string
	WorkingDir string
	Labels     map[string]string
	Memory     int64
	AutoRemove bool
	// Binds are "VOLUME:/path[:ro|:rw]": named volumes only (a host path
	// means nothing to a WebAssembly engine), created if missing.
	Binds []string
	// Network is the network to join by name or ID ("" or "default": the
	// engine's default; "none": no network).
	Network string
	// Caps are device capabilities granted to the container ("gpu").
	Caps []string
	// Restart is the restart policy (HostConfig.RestartPolicy).
	Restart container.RestartPolicy
	// Endpoints are the networks' aliases by network (Docker's
	// NetworkingConfig.EndpointsConfig); a network named there and not in
	// Network is joined too (API 1.44+), and with no Network the first of
	// them by name is the primary.
	Endpoints map[string][]string
	// Ports are published ports (HostConfig.PortBindings, run -p);
	// ExposedPorts adds ports with no binding (Config.ExposedPorts, run
	// --expose), to the image's own; PublishAll gives each of those a port
	// the system picks (run -P).
	Ports        portmap.Map
	ExposedPorts map[string]struct{}
	PublishAll   bool
}

// ParseBind reads "VOLUME:/path[:ro|:rw]".
func ParseBind(b string) (container.Mount, error) {
	parts := strings.Split(b, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return container.Mount{}, fmt.Errorf("%w: invalid volume specification %q (want VOLUME:/path[:ro])", container.ErrInvalid, b)
	}
	m := container.Mount{Volume: parts[0], Target: parts[1]}
	if strings.HasPrefix(m.Volume, "/") || strings.HasPrefix(m.Volume, ".") {
		return m, fmt.Errorf("%w: %q is a host path: this engine mounts named volumes only (VOLUME:/path)", container.ErrInvalid, m.Volume)
	}
	if !volume.ValidName(m.Volume) {
		return m, fmt.Errorf("%w: invalid volume name %q", container.ErrInvalid, m.Volume)
	}
	if len(parts) == 3 {
		switch parts[2] {
		case "ro":
			m.ReadOnly = true
		case "rw":
		default:
			return m, fmt.Errorf("%w: invalid mode %q in %q (ro or rw)", container.ErrInvalid, parts[2], b)
		}
	}
	return m, nil
}

// CreateVolume makes a volume (or returns the existing one).
func (d *Daemon) CreateVolume(name string, labels map[string]string) (*volume.Volume, error) {
	v, created, err := d.vols.Create(name, labels)
	if err == nil && created {
		d.events.Publish(events.Event{Type: events.Volume, Action: "create", ID: v.Name, Attributes: map[string]string{"driver": "local"}})
	}
	return v, err
}

// RemoveVolume deletes a volume no container uses; with force a missing
// one is not an error (Docker's meaning of force for volumes).
func (d *Daemon) RemoveVolume(name string, force bool) error {
	err := d.vols.Remove(name, d.mgr.UsesVolume)
	if errors.Is(err, volume.ErrNotFound) && force {
		return nil
	}
	if err == nil {
		d.events.Publish(events.Event{Type: events.Volume, Action: "destroy", ID: name, Attributes: map[string]string{"driver": "local"}})
	}
	return err
}

// PruneVolumes removes unused anonymous volumes, or with all every
// unused one.
func (d *Daemon) PruneVolumes(all bool) ([]string, int64) {
	gone, n := d.vols.Prune(all, d.mgr.UsesVolume)
	for _, g := range gone {
		d.events.Publish(events.Event{Type: events.Volume, Action: "destroy", ID: g, Attributes: map[string]string{"driver": "local"}})
	}
	d.events.Publish(events.Event{Type: events.Volume, Action: "prune", Attributes: map[string]string{"reclaimed": strconv.FormatInt(n, 10)}})
	return gone, n
}

// ExecConfig returns the run configuration an image declares. A wasm
// artifact has none; its single module is the entrypoint.
func ExecConfig(img *image.Image) image.ExecConfig {
	c := img.Config.Config
	if img.Wasm && len(c.Entrypoint) == 0 && len(img.Manifest.Layers) == 1 {
		c.Entrypoint = []string{rootfs.ModulePath(img.Manifest.Layers[0].Annotations[image.AnnotationTitle])}
	}
	return c
}

func mergeEnv(base, over []string) []string {
	out := append([]string{}, base...)
	for _, kv := range over {
		k, _, _ := strings.Cut(kv, "=")
		replaced := false
		for i, b := range out {
			if bk, _, _ := strings.Cut(b, "="); bk == k {
				out[i] = kv
				replaced = true
			}
		}
		if !replaced {
			out = append(out, kv)
		}
	}
	return out
}

// containerPorts is a container's ports: the image's exposed ports, the
// request's, and the request's bindings; nil when there are none.
func containerPorts(image map[string]struct{}, req CreateRequest) (portmap.Map, error) {
	m := portmap.Map{}
	if err := m.Exposed(image); err != nil {
		return nil, fmt.Errorf("%w: the image's ExposedPorts: %v", container.ErrInvalid, err)
	}
	if err := m.Exposed(req.ExposedPorts); err != nil {
		return nil, fmt.Errorf("%w: %v", container.ErrInvalid, err)
	}
	m.Merge(req.Ports)
	if m.Bindings() > portmap.MaxBindings {
		return nil, fmt.Errorf("%w: at most %d published ports", container.ErrInvalid, portmap.MaxBindings)
	}
	if len(m) == 0 {
		return nil, nil
	}
	return m, nil
}

// CreateContainer makes a container from an image.
func (d *Daemon) CreateContainer(req CreateRequest) (*container.Container, error) {
	name, img, err := d.ResolveImage(req.Image)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = req.Image
	}
	lower, err := d.lower(img)
	if err != nil {
		return nil, err
	}
	ec := ExecConfig(img)
	cfg := container.Config{
		Image: req.Image, ImageID: string(img.ID()),
		Entrypoint: ec.Entrypoint, Cmd: ec.Cmd,
		Env: mergeEnv(ec.Env, req.Env), WorkingDir: ec.WorkingDir, Labels: ec.Labels,
		MemoryLimitBytes: req.Memory, AutoRemove: req.AutoRemove, Caps: req.Caps, Restart: req.Restart,
	}
	if req.Entrypoint != nil {
		cfg.Entrypoint, cfg.Cmd = req.Entrypoint, nil
	}
	if req.Cmd != nil {
		cfg.Cmd = req.Cmd
	}
	if req.WorkingDir != "" {
		cfg.WorkingDir = req.WorkingDir
	}
	if cfg.Networks, err = d.attachments(req.Network, req.Endpoints); err != nil {
		return nil, err
	}
	if cfg.Ports, err = containerPorts(ec.ExposedPorts, req); err != nil {
		return nil, err
	}
	cfg.PublishAll = req.PublishAll
	if len(req.Labels) > 0 {
		cfg.Labels = req.Labels
	}
	for _, b := range req.Binds {
		mt, err := ParseBind(b)
		if err != nil {
			return nil, err
		}
		cfg.Mounts = append(cfg.Mounts, mt)
	}
	// Named volumes are created on first use, as Docker does; after the
	// mounts are known to be well formed.
	for _, mt := range cfg.Mounts {
		if _, err := d.CreateVolume(mt.Volume, nil); err != nil {
			return nil, err
		}
	}
	return d.mgr.Create(container.CreateOptions{Name: req.Name, Config: cfg, Lower: lower})
}

// DeleteItem is one line of an image removal.
type DeleteItem struct {
	Untagged string
	Deleted  string
}

// RemoveImage untags ref (or every name of an image given by ID) and
// deletes the image's blobs once no name is left. An image a container
// uses is refused unless force.
func (d *Daemon) RemoveImage(ref string, force bool) ([]DeleteItem, error) {
	name, img, err := d.ResolveImage(ref)
	if err != nil {
		return nil, err
	}
	out, err := d.removeImage(ref, name, img, force)
	d.emitRemoved(out, string(img.ID()))
	return out, err
}

func (d *Daemon) removeImage(ref, name string, img *image.Image, force bool) ([]DeleteItem, error) {
	if users := d.mgr.UsesImage(string(img.ID())); len(users) > 0 && !force {
		return nil, fmt.Errorf("%w: unable to remove %s (must force) - container %s is using it", ErrImageInUse, ref, users[0][:12])
	}
	var names []string
	if name != "" {
		names = []string{name}
	} else {
		for _, nr := range d.store.Names() {
			if i, err := d.store.Image(nr.Manifest, d.opt.Limits); err == nil && i.ID() == img.ID() {
				names = append(names, nr.Name)
			}
		}
	}
	var out []DeleteItem
	for _, n := range names {
		if _, err := d.store.Untag(n); err != nil {
			return out, err
		}
		if !image.IsDangling(n) {
			out = append(out, DeleteItem{Untagged: n})
		}
	}
	// Still named elsewhere? Then nothing is deleted.
	for _, nr := range d.store.Names() {
		if i, err := d.store.Image(nr.Manifest, d.opt.Limits); err == nil && i.ID() == img.ID() {
			return out, nil
		}
	}
	gone, err := d.store.GC(d.opt.Limits)
	if err != nil {
		return out, err
	}
	d.mu.Lock()
	delete(d.lowers, img.ManifestDigest)
	d.mu.Unlock()
	if len(gone) > 0 && d.opt.ImagesRemoved != nil {
		d.opt.ImagesRemoved()
	}
	for _, g := range gone {
		if g == img.ID() {
			out = append(out, DeleteItem{Deleted: string(g)})
		}
	}
	for _, g := range gone {
		if g != img.ID() {
			out = append(out, DeleteItem{Deleted: string(g)})
		}
	}
	return out, nil
}

// emitRemoved publishes untag and delete events for a removal.
func (d *Daemon) emitRemoved(items []DeleteItem, id string) {
	for _, it := range items {
		if it.Untagged != "" {
			d.events.Publish(events.Event{Type: events.Image, Action: "untag", ID: id, Attributes: map[string]string{"name": it.Untagged}})
		}
		if it.Deleted != "" {
			d.events.Publish(events.Event{Type: events.Image, Action: "delete", ID: it.Deleted, Attributes: map[string]string{"name": it.Deleted}})
		}
	}
}
