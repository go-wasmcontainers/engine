// Package container is the container lifecycle: created, running, exited;
// its root filesystem, its logs, its exit code. It runs modules through a
// runtime.Executor and knows nothing of images beyond the frozen lower
// filesystem and the run configuration it is handed.
package container

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/state"
)

// State is a container's lifecycle state.
type State string

const (
	Created State = "created"
	Running State = "running"
	Exited  State = "exited"
)

// Errors the API layer maps to HTTP statuses.
var (
	ErrNotFound       = errors.New("no such container")
	ErrNameConflict   = errors.New("container name already in use")
	ErrRunning        = errors.New("container is running")
	ErrNotRunning     = errors.New("container is not running")
	ErrAlreadyStarted = errors.New("container already started")
	ErrInvalid        = errors.New("invalid container configuration")
	ErrTooMany        = errors.New("too many containers")
	ErrAmbiguous      = errors.New("multiple containers match")
)

// Config is how a container runs, resolved from the image config and the
// run overrides.
type Config struct {
	Image      string            `json:"Image"`
	ImageID    string            `json:"-"`
	Entrypoint []string          `json:"Entrypoint"`
	Cmd        []string          `json:"Cmd"`
	Env        []string          `json:"Env"`
	WorkingDir string            `json:"WorkingDir"`
	Labels     map[string]string `json:"Labels"`
	// MemoryLimitBytes bounds linear memory (0 = manager default).
	MemoryLimitBytes int64 `json:"-"`
	// AutoRemove removes the container when it exits (run --rm).
	AutoRemove bool `json:"-"`
}

// Container is one container. Read it through Snapshot.
type Container struct {
	id      string
	name    string
	created time.Time
	cfg     Config
	fs      *rootfs.FS
	logs    *Logs

	mu         sync.Mutex
	state      State
	exitCode   int
	errMsg     string
	timedOut   bool
	startedAt  time.Time
	finishedAt time.Time
	exits      int
	removed    bool
	path       string
	proc       runtime.Process
	changed    chan struct{}

	persisted persisted // guarded by Manager.pmu
	queued    bool      // guarded by Manager.qmu
}

// Snapshot is a consistent copy of a container's state.
type Snapshot struct {
	ID         string
	Name       string
	Created    time.Time
	Config     Config
	State      State
	ExitCode   int
	Error      string
	TimedOut   bool
	StartedAt  time.Time
	FinishedAt time.Time
	// Path is the resolved module path (empty until the first start).
	Path string
	// SizeRw is the bytes the container wrote to its filesystem.
	SizeRw int64
}

// Command is argv as configured.
func (s Snapshot) Command() []string {
	return append(append([]string{}, s.Config.Entrypoint...), s.Config.Cmd...)
}

// Snapshot returns the container's current state.
func (c *Container) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	rw, _ := c.fs.Usage()
	return Snapshot{
		ID: c.id, Name: c.name, Created: c.created, Config: c.cfg,
		State: c.state, ExitCode: c.exitCode, Error: c.errMsg, TimedOut: c.timedOut,
		StartedAt: c.startedAt, FinishedAt: c.finishedAt, Path: c.path, SizeRw: rw,
	}
}

// ID returns the container's ID.
func (c *Container) ID() string { return c.id }

// FS returns the container's root filesystem.
func (c *Container) FS() *rootfs.FS { return c.fs }

func (c *Container) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Options configures a Manager.
type Options struct {
	Executor runtime.Executor
	// MemoryLimitBytes is the default linear-memory bound per container.
	MemoryLimitBytes int64
	// Timeout is the wall-clock bound of one run (0 = none).
	Timeout time.Duration
	// Quota bounds what one container may write to its filesystem.
	Quota rootfs.Quota
	// MaxLogBytes bounds one container's kept output (default 8 MiB).
	MaxLogBytes int
	// MaxContainers bounds how many containers exist (default 256).
	MaxContainers int
	// Store, when set, keeps containers across restarts: their state,
	// logs and writable layers (see Restore).
	Store state.Store
	// CheckpointEvery is how often a running container's changes are
	// stored (default 2s; negative: only at state changes).
	CheckpointEvery time.Duration
}

// Manager owns every container of a daemon.
type Manager struct {
	opt Options

	mu         sync.Mutex
	byID       map[string]*Container
	byName     map[string]*Container
	persistErr error

	pmu   sync.Mutex     // serialises writes to opt.Store
	exits sync.WaitGroup // exits not yet stored (or --rm removals not done)

	// The writer: containers whose state changed, stored in the
	// background so no command waits on storage (see persister).
	qmu      sync.Mutex
	queue    []*Container
	flushers []chan struct{}
	wake     chan struct{}
}

// NewManager returns a manager running modules on opt.Executor.
func NewManager(opt Options) *Manager {
	if opt.MaxLogBytes <= 0 {
		opt.MaxLogBytes = 8 << 20
	}
	if opt.MaxContainers <= 0 {
		opt.MaxContainers = 256
	}
	if opt.Quota.MaxBytes == 0 {
		opt.Quota.MaxBytes = 256 << 20
	}
	if opt.Quota.MaxFiles == 0 {
		opt.Quota.MaxFiles = 1 << 16
	}
	m := &Manager{opt: opt, byID: map[string]*Container{}, byName: map[string]*Container{}, wake: make(chan struct{}, 1)}
	if opt.Store != nil {
		go m.persister()
	}
	return m
}

// nameRE is Docker's restricted name grammar.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

var (
	adjectives = []string{"brave", "calm", "eager", "fancy", "gentle", "happy", "jolly", "keen", "lucid", "merry", "nifty", "proud", "quiet", "sharp", "tidy", "witty"}
	nouns      = []string{"lovelace", "turing", "hopper", "knuth", "ritchie", "thompson", "liskov", "hamilton", "dijkstra", "backus", "kay", "lamport", "torvalds", "pike", "wirth", "allen"}
)

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	return hex.EncodeToString(b)
}

func randomName() string {
	b := make([]byte, 2)
	rand.Read(b)
	return adjectives[int(b[0])%len(adjectives)] + "_" + nouns[int(b[1])%len(nouns)]
}

// CreateOptions describes a new container.
type CreateOptions struct {
	Name   string
	Config Config
	// Lower is the image's frozen root filesystem.
	Lower *rootfs.FS
}

// Create makes a container in the created state.
func (m *Manager) Create(o CreateOptions) (*Container, error) {
	c, err := m.create(o)
	if err == nil {
		m.schedule(c)
	}
	return c, err
}

func (m *Manager) create(o CreateOptions) (*Container, error) {
	if o.Lower == nil {
		return nil, fmt.Errorf("%w: no root filesystem", ErrInvalid)
	}
	if len(o.Config.Entrypoint)+len(o.Config.Cmd) == 0 {
		return nil, fmt.Errorf("%w: no command specified", ErrInvalid)
	}
	name := strings.TrimPrefix(o.Name, "/")
	if name != "" && (!nameRE.MatchString(name) || len(name) > 128) {
		return nil, fmt.Errorf("%w: invalid container name %q, only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed", ErrInvalid, o.Name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.byID) >= m.opt.MaxContainers {
		return nil, fmt.Errorf("%w: limit is %d", ErrTooMany, m.opt.MaxContainers)
	}
	if name == "" {
		for i := 0; ; i++ {
			name = randomName()
			if i > 8 {
				name += "_" + randomHex(2)
			}
			if m.byName[name] == nil {
				break
			}
		}
	} else if m.byName[name] != nil {
		return nil, fmt.Errorf("%w: %q is used by %s", ErrNameConflict, name, m.byName[name].id[:12])
	}
	c := &Container{
		id: randomHex(32), name: name, created: time.Now().UTC(), cfg: o.Config,
		fs: o.Lower.Clone(m.opt.Quota), logs: newLogs(m.opt.MaxLogBytes),
		state: Created, changed: make(chan struct{}),
	}
	m.byID[c.id] = c
	m.byName[name] = c
	return c, nil
}

// Get finds a container by full ID, name, or unique ID prefix.
func (m *Manager) Get(ref string) (*Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ref == "" {
		return nil, fmt.Errorf("%w: %q", ErrNotFound, ref)
	}
	if c := m.byID[ref]; c != nil {
		return c, nil
	}
	if c := m.byName[strings.TrimPrefix(ref, "/")]; c != nil {
		return c, nil
	}
	var found *Container
	for id, c := range m.byID {
		if strings.HasPrefix(id, ref) {
			if found != nil {
				return nil, fmt.Errorf("%w: %q", ErrAmbiguous, ref)
			}
			found = c
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	return found, nil
}

// List returns containers, newest first; all includes stopped ones.
func (m *Manager) List(all bool) []Snapshot {
	m.mu.Lock()
	cs := make([]*Container, 0, len(m.byID))
	for _, c := range m.byID {
		cs = append(cs, c)
	}
	m.mu.Unlock()
	out := make([]Snapshot, 0, len(cs))
	for _, c := range cs {
		s := c.Snapshot()
		if all || s.State == Running {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Created.Equal(out[j].Created) {
			return out[i].Created.After(out[j].Created)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// UsesImage reports whether any container was created from imageID.
func (m *Manager) UsesImage(imageID string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, c := range m.byID {
		if c.cfg.ImageID == imageID {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

const defaultPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func envValue(env []string, key string) (string, bool) {
	for i := len(env) - 1; i >= 0; i-- {
		if k, v, ok := strings.Cut(env[i], "="); ok && k == key {
			return v, true
		}
	}
	return "", false
}

// resolveModule finds argv[0] in the container's filesystem: an absolute
// path as is, a relative path from the working directory, a bare name on
// $PATH (trying name and name.wasm).
func resolveModule(fs *rootfs.FS, argv0, workdir string, env []string) string {
	if strings.HasPrefix(argv0, "/") {
		return path.Clean(argv0)
	}
	if strings.Contains(argv0, "/") {
		return path.Join("/", workdir, argv0)
	}
	pv, ok := envValue(env, "PATH")
	if !ok {
		pv = defaultPath
	}
	for _, dir := range strings.Split(pv, ":") {
		if dir == "" {
			continue
		}
		for _, n := range []string{argv0, argv0 + ".wasm"} {
			p := path.Join("/", dir, n)
			if st, err := fs.Stat(p); err == nil && st.Mode().IsRegular() {
				return p
			}
		}
	}
	return path.Join("/", argv0)
}

// Start runs a created or exited container.
func (m *Manager) Start(ctx context.Context, ref string) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.state == Running {
		c.mu.Unlock()
		return ErrAlreadyStarted
	}
	argv := append(append([]string{}, c.cfg.Entrypoint...), c.cfg.Cmd...)
	env := append([]string{}, c.cfg.Env...)
	if _, ok := envValue(env, "PATH"); !ok {
		env = append([]string{"PATH=" + defaultPath}, env...)
	}
	if _, ok := envValue(env, "HOSTNAME"); !ok {
		env = append(env, "HOSTNAME="+c.id[:12])
	}
	// Go's wasip1 port takes its working directory from $PWD (else the
	// first preopen); set it to WorkingDir, "/" by default.
	if _, ok := envValue(env, "PWD"); !ok {
		env = append(env, "PWD="+path.Clean("/"+c.cfg.WorkingDir))
	}
	c.path = resolveModule(c.fs, argv[0], c.cfg.WorkingDir, env)
	mem := c.cfg.MemoryLimitBytes
	if mem <= 0 {
		mem = m.opt.MemoryLimitBytes
	}
	spec := runtime.Spec{
		ID: c.id, FS: c.fs, Path: c.path, Args: argv, Env: env,
		Stdout: logWriter{c.logs, Stdout}, Stderr: logWriter{c.logs, Stderr},
		MemoryLimitBytes: mem, Timeout: m.opt.Timeout,
	}
	c.state = Running
	c.startedAt = time.Now().UTC()
	c.finishedAt = time.Time{}
	c.errMsg, c.exitCode, c.timedOut = "", 0, false
	c.notifyLocked()
	c.mu.Unlock()
	m.schedule(c)

	proc, err := m.opt.Executor.Start(ctx, spec)
	if err != nil {
		code := runtime.ExitNoExec
		if errors.Is(err, runtime.ErrNotFound) {
			code = runtime.ExitMissing
		}
		c.finish(runtime.Result{ExitCode: code, Error: err.Error()})
		m.exited(c)
		return err
	}
	c.mu.Lock()
	c.proc = proc
	c.mu.Unlock()
	go m.checkpointWhileRunning(c)
	m.exits.Add(1)
	go func() {
		defer m.exits.Done()
		c.finish(proc.Wait())
		m.exited(c)
	}()
	return nil
}

// exited stores an exit, or removes a --rm container.
func (m *Manager) exited(c *Container) {
	if c.cfg.AutoRemove {
		m.removeNow(c)
		return
	}
	m.schedule(c)
}

// removeNow takes a container that is not running out of the manager and
// the store.
func (m *Manager) removeNow(c *Container) {
	m.mu.Lock()
	if m.byID[c.id] == c {
		delete(m.byID, c.id)
		delete(m.byName, c.name)
	}
	m.mu.Unlock()
	m.schedule(c) // no longer live: the writer deletes it
	c.mu.Lock()
	c.removed = true
	c.notifyLocked()
	c.mu.Unlock()
}

// Prune removes every container that is not running and returns their IDs
// and the bytes their writable layers held.
func (m *Manager) Prune() ([]string, int64) {
	var ids []string
	var bytes int64
	for _, s := range m.List(true) {
		if s.State == Running {
			continue
		}
		c, err := m.Get(s.ID)
		if err != nil {
			continue
		}
		c.mu.Lock()
		running := c.state == Running
		c.mu.Unlock()
		if running {
			continue
		}
		m.removeNow(c)
		ids = append(ids, s.ID)
		bytes += s.SizeRw
	}
	return ids, bytes
}

func (c *Container) finish(r runtime.Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.state = Exited
	c.exitCode = r.ExitCode
	c.errMsg = r.Error
	c.timedOut = r.TimedOut
	c.finishedAt = time.Now().UTC()
	c.proc = nil
	c.exits++
	c.notifyLocked()
}

// Kill ends a running container.
func (m *Manager) Kill(ref string) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	p := c.proc
	running := c.state == Running
	c.mu.Unlock()
	if !running {
		return ErrNotRunning
	}
	if p != nil {
		p.Kill()
	}
	return nil
}

// WaitCondition is a /containers/{id}/wait condition.
type WaitCondition string

const (
	WaitNotRunning WaitCondition = "not-running"
	WaitNextExit   WaitCondition = "next-exit"
	WaitRemoved    WaitCondition = "removed"
)

// Wait blocks until the condition holds and returns the exit code.
func (m *Manager) Wait(ctx context.Context, ref string, cond WaitCondition) (int, error) {
	c, err := m.Get(ref)
	if err != nil {
		return 0, err
	}
	return c.Wait(ctx, cond)
}

// Wait blocks until the condition holds and returns the exit code.
func (c *Container) Wait(ctx context.Context, cond WaitCondition) (int, error) {
	c.mu.Lock()
	startExits := c.exits
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var ok bool
		switch cond {
		case WaitNextExit:
			ok = c.exits > startExits
		case WaitRemoved:
			ok = c.removed
		case WaitNotRunning, "":
			ok = c.state != Running
		default:
			c.mu.Unlock()
			return 0, fmt.Errorf("%w: wait condition %q", ErrInvalid, cond)
		}
		code, ch := c.exitCode, c.changed
		c.mu.Unlock()
		if ok {
			return code, nil
		}
		select {
		case <-ch:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
}

// Remove deletes a container; a running one only with force, which kills it.
func (m *Manager) Remove(ctx context.Context, ref string, force bool) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	running, p := c.state == Running, c.proc
	c.mu.Unlock()
	if running {
		if !force {
			return fmt.Errorf("%w: stop the container before removing it or force remove", ErrRunning)
		}
		if p != nil {
			p.Kill()
		}
		if _, err := c.Wait(ctx, WaitNotRunning); err != nil {
			return err
		}
	}
	m.removeNow(c)
	return nil
}

// Logs streams the container's output to fn: everything kept so far and,
// with follow, everything written until the container stops running.
func (c *Container) Logs(ctx context.Context, follow bool, fn func(Record) error) error {
	seq := 0
	for {
		// Read the state before the records: a stop seen here means every
		// record written before it is already in the log.
		c.mu.Lock()
		running := c.state == Running || (c.state == Created && follow)
		stateCh := c.changed
		c.mu.Unlock()
		recs, next, logCh := c.logs.since(seq)
		for _, r := range recs {
			if err := fn(r); err != nil {
				return err
			}
		}
		seq = next
		if !follow || !running {
			return nil
		}
		select {
		case <-logCh:
		case <-stateCh:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Logs streams a container's output; see Container.Logs.
func (m *Manager) Logs(ctx context.Context, ref string, follow bool, fn func(Record) error) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	return c.Logs(ctx, follow, fn)
}

// Shutdown kills every running container and waits for them.
func (m *Manager) Shutdown(ctx context.Context) {
	m.mu.Lock()
	cs := make([]*Container, 0, len(m.byID))
	for _, c := range m.byID {
		cs = append(cs, c)
	}
	m.mu.Unlock()
	for _, c := range cs {
		c.mu.Lock()
		p := c.proc
		c.mu.Unlock()
		if p != nil {
			p.Kill()
			c.Wait(ctx, WaitNotRunning)
		}
	}
	// Their exits are stored (or --rm ones removed) before Shutdown
	// returns, so the next engine on the same store sees them settled.
	done := make(chan struct{})
	go func() { m.exits.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	m.Flush(ctx)
}
