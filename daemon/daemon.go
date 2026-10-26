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
	"strings"
	"sync"
	"time"

	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/rootfs"
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
}

// Daemon is one engine instance.
type Daemon struct {
	opt   Options
	store *image.Store
	mgr   *container.Manager

	mu     sync.Mutex
	lowers map[image.Digest]*lowerEntry // by manifest digest
	tick   uint64
}

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
	return &Daemon{opt: opt, store: st, mgr: container.NewManager(opt.Containers), lowers: map[image.Digest]*lowerEntry{}}, nil
}

// Containers is the container manager.
func (d *Daemon) Containers() *container.Manager { return d.mgr }

// Close kills running containers.
func (d *Daemon) Close(ctx context.Context) error {
	d.mgr.Shutdown(ctx)
	return nil
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
		MemoryLimitBytes: req.Memory,
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
	if len(req.Labels) > 0 {
		cfg.Labels = req.Labels
	}
	c, err := d.mgr.Create(container.CreateOptions{Name: req.Name, Config: cfg, Lower: lower})
	if err != nil {
		return nil, err
	}
	if req.AutoRemove {
		go func() {
			if _, err := c.Wait(context.Background(), container.WaitNextExit); err == nil {
				d.mgr.Remove(context.Background(), c.ID(), true)
			}
		}()
	}
	return c, nil
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
		out = append(out, DeleteItem{Untagged: n})
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
