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
	"net/netip"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-wasmcontainers/engine/events"
	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/portmap"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/state"
	"github.com/go-wasmcontainers/engine/wasi"
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
	// Mounts are the volumes it mounts (run -v NAME:/path[:ro]).
	Mounts []Mount `json:"-"`
	// Networks are the networks it joins (run --network and
	// --network-alias, network connect), the first one its primary: empty
	// for the engine's default, [{none}] for no network.
	Networks []Attachment `json:"-"`
	// Caps are device capabilities granted to it (run --gpu: "gpu").
	Caps []string `json:"-"`
	// Ports are its exposed ports and their host bindings (run -p,
	// --expose, the image's ExposedPorts); PublishAll gives each exposed
	// port without a binding one the system picks (run -P).
	Ports      portmap.Map `json:"-"`
	PublishAll bool        `json:"-"`
	// Restart is its restart policy (run --restart; see restart.go).
	Restart RestartPolicy `json:"-"`
	// Tty gives its first process a terminal (run -t); OpenStdin a stdin
	// that attached clients write into (run -i); StdinOnce closes that
	// stdin when the first attached client's input ends (stdio.go).
	// ConsoleSize is the terminal's first size, height then width (0, 0:
	// 80x24).
	Tty         bool      `json:"-"`
	OpenStdin   bool      `json:"-"`
	StdinOnce   bool      `json:"-"`
	ConsoleSize [2]uint16 `json:"-"`
	// Healthcheck is its health check (the image's, or run --health-*;
	// see health.go); nil or NONE for none.
	Healthcheck *HealthConfig `json:"-"`
}

// published is what Start publishes: the bindings, with run -P applied.
func (cfg Config) published() portmap.Map {
	if cfg.PublishAll {
		return cfg.Ports.PublishAll()
	}
	return cfg.Ports
}

// Mount is a volume mounted at Target.
type Mount struct {
	Volume   string `json:"volume"`
	Target   string `json:"target"`
	ReadOnly bool   `json:"readOnly,omitempty"`
}

// checkMounts refuses targets that are not absolute and clean, the root,
// or used twice.
func checkMounts(ms []Mount) error {
	seen := map[string]bool{}
	for _, mt := range ms {
		if mt.Volume == "" || !strings.HasPrefix(mt.Target, "/") || path.Clean(mt.Target) != mt.Target || mt.Target == "/" || len(mt.Target) > 4096 {
			return fmt.Errorf("%w: invalid mount %s:%s (the target must be an absolute, clean path other than /)", ErrInvalid, mt.Volume, mt.Target)
		}
		if seen[mt.Target] {
			return fmt.Errorf("%w: duplicate mount point: %s", ErrInvalid, mt.Target)
		}
		seen[mt.Target] = true
	}
	return nil
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
	// dies counts the "die" events published: a waiter for not-running
	// returns once the exit's die is out, so `stop` follows `die`, as in
	// Docker's event stream.
	dies    int
	removed bool
	path    string
	ip      netip.Addr
	net     *netdriver.Multi // its networks while it runs
	ports   *portmap.Set     // its published ports while it runs
	proc    runtime.Process
	// stdin and term are the running process's (stdio.go), nil without.
	stdin *stdinHub
	term  *wasi.Terminal
	// killPending is a kill that arrived while running but before proc
	// was attached; Start delivers it.
	killPending bool
	changed     chan struct{}
	// The restart policy's state (restart.go): stopped by a user, restarts
	// so far, the current delay, and the pending restart.
	manualStop   bool
	restartCount int
	restartDelay time.Duration
	restartTimer *time.Timer
	// health is its health state while a check runs (health.go).
	health *Health

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
	// IPAddress is its address on its primary network while it runs.
	IPAddress netip.Addr
	// Networks are its networks, with their addresses while it runs.
	Networks []NetworkInfo
	// Ports are the published ports while it runs, as bound (a picked
	// host port as picked); nil otherwise.
	Ports portmap.Map
	// RestartCount is how many times its policy restarted it since a user
	// last started it.
	RestartCount int
	// Health is its health state, nil without a check.
	Health *Health
}

// Command is argv as configured.
func (s Snapshot) Command() []string {
	return append(append([]string{}, s.Config.Entrypoint...), s.Config.Cmd...)
}

// Stats is what a container uses now. CPU time is not in it: a wasm guest
// gives the engine no CPU accounting (its wall-clock time includes the
// time it waits on the host), and an estimate would be a guess.
type Stats struct {
	Read    time.Time
	Running bool
	// MemoryBytes is the guest's linear memory (0 when not running);
	// MemoryLimitBytes the most it may grow to.
	MemoryBytes, MemoryLimitBytes int64
	// WrittenBytes is everything written to its filesystem, ever;
	// SizeRw what its writable layer holds now.
	WrittenBytes, SizeRw int64
	// LogBytes is the output kept.
	LogBytes int64
}

// DefaultMemoryLimit is the memory limit of containers that set none.
func (m *Manager) DefaultMemoryLimit() int64 { return m.opt.MemoryLimitBytes }

// Stats measures the container now.
func (c *Container) Stats() Stats {
	c.mu.Lock()
	p, running := c.proc, c.state == Running
	limit := c.cfg.MemoryLimitBytes
	c.mu.Unlock()
	s := Stats{Read: time.Now().UTC(), Running: running, MemoryLimitBytes: limit, WrittenBytes: c.fs.Written(), LogBytes: c.logs.bytes()}
	s.SizeRw, _ = c.fs.Usage()
	if r, ok := p.(runtime.StatsReporter); ok && running && p != nil {
		rs := r.Stats()
		s.MemoryBytes, s.MemoryLimitBytes = rs.MemoryBytes, rs.MemoryLimitBytes
	}
	return s
}

// Snapshot returns the container's current state.
func (c *Container) Snapshot() Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	rw, _ := c.fs.Usage()
	s := Snapshot{
		ID: c.id, Name: c.name, Created: c.created, Config: c.cfg,
		State: c.state, ExitCode: c.exitCode, Error: c.errMsg, TimedOut: c.timedOut,
		StartedAt: c.startedAt, FinishedAt: c.finishedAt, Path: c.path, SizeRw: rw, IPAddress: c.ip,
		Networks: c.networksLocked(), RestartCount: c.restartCount, Health: c.healthLocked(),
	}
	if c.ports != nil {
		s.Ports = c.ports.Ports()
	}
	return s
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
	// Events, when set, receives create, start, die, kill, stop, restart,
	// destroy and prune, with Docker's attributes (image, name, labels,
	// exitCode).
	Events *events.Bus
	// Volume finds a volume's tree by name, for Config.Mounts.
	Volume func(name string) (*rootfs.FS, error)
	// Network attaches a container to one of Config.Networks under its DNS
	// name and aliases; nil means containers have no network.
	Network func(ctx context.Context, network, name string, aliases []string) (netdriver.Endpoint, error)
	// Ports is where published ports listen (wctrd: portmap.OS); nil
	// means this engine cannot publish them, and a container that asks
	// is refused at start.
	Ports portmap.Host
	// PortLimits bound each published port (zero: portmap.DefaultLimits).
	PortLimits portmap.Limits
}

var networkRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// ValidNetworkName reports a network name Docker would accept.
func ValidNetworkName(s string) bool { return networkRE.MatchString(s) }

// DNSName is the name a container is reachable by on its network: its
// name when that is a DNS label, its short ID otherwise.
func DNSName(name, id string) string {
	if n := strings.ToLower(name); netdriver.ValidName(n) {
		return n
	}
	if len(id) > 12 {
		id = id[:12]
	}
	return id
}

// emit publishes a container event.
func (m *Manager) emit(action string, c *Container, extra map[string]string) {
	if m.opt.Events == nil {
		return
	}
	attrs := map[string]string{"image": c.cfg.Image, "name": c.name}
	for k, v := range c.cfg.Labels {
		attrs[k] = v
	}
	for k, v := range extra {
		attrs[k] = v
	}
	m.opt.Events.Publish(events.Event{Type: events.Container, Action: action, ID: c.id, Attributes: attrs})
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

	execs execTable // exec instances (exec.go)

	// stopping is set by Shutdown: no restart policy acts any more.
	stopping atomic.Bool
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
		m.emit("create", c, nil)
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
	if err := checkHealth(o.Config.Healthcheck); err != nil {
		return nil, err
	}
	if err := checkRestart(o.Config); err != nil {
		return nil, err
	}
	if err := checkPorts(o.Config); err != nil {
		return nil, err
	}
	if err := checkMounts(o.Config.Mounts); err != nil {
		return nil, err
	}
	atts, err := checkAttachments(o.Config.Networks)
	if err != nil {
		return nil, err
	}
	o.Config.Networks = atts
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
	// The mount points, so the guest sees them in their parent directory.
	for _, mt := range o.Config.Mounts {
		if err := c.fs.MkdirAll(mt.Target, 0o755); err != nil {
			return nil, fmt.Errorf("%w: mount point %s: %v", ErrInvalid, mt.Target, err)
		}
	}
	m.byID[c.id] = c
	m.byName[name] = c
	return c, nil
}

// UsesVolume lists the containers, in any state, that mount a volume.
func (m *Manager) UsesVolume(name string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var ids []string
	for id, c := range m.byID {
		for _, mt := range c.cfg.Mounts {
			if mt.Volume == name {
				ids = append(ids, id)
				break
			}
		}
	}
	sort.Strings(ids)
	return ids
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
		if all || s.State == Running || s.State == Restarting {
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

// Start runs a created or exited container (a user's start: its restart
// policy acts again, counting from zero).
func (m *Manager) Start(ctx context.Context, ref string) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	return m.start(ctx, c, false)
}

// start runs c; auto is a restart its policy asked for.
func (m *Manager) start(ctx context.Context, c *Container, auto bool) error {
	c.mu.Lock()
	if c.state == Running {
		c.mu.Unlock()
		return ErrAlreadyStarted
	}
	c.cancelRestartLocked()
	if !auto {
		c.manualStop, c.restartCount, c.restartDelay = false, 0, 0
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
		MemoryLimitBytes: mem, Timeout: m.opt.Timeout, Caps: c.cfg.Caps,
	}
	for _, mt := range c.cfg.Mounts {
		var vfs *rootfs.FS
		var err error
		if m.opt.Volume == nil {
			err = errors.New("no volumes in this engine")
		} else {
			vfs, err = m.opt.Volume(mt.Volume)
		}
		if err != nil {
			return m.failStart(c, fmt.Errorf("%w: volume %s: %v", ErrInvalid, mt.Volume, err))
		}
		spec.Mounts = append(spec.Mounts, runtime.Mount{Path: mt.Target, FS: vfs, ReadOnly: mt.ReadOnly})
	}
	ep, err := m.attach(ctx, c)
	if err != nil {
		return m.failStart(c, err)
	}
	ports, err := m.publish(ctx, c, ep)
	if err != nil {
		if ep != nil {
			ep.Detach()
		}
		return m.failStart(c, err)
	}
	c.net = ep
	c.ports = ports
	if ep != nil { // nil: no network
		spec.Net = netdriver.WithDNS(ep)
		c.ip = ep.Addr()
	}
	var endStdio func()
	{
		in, out, errw, term, end := c.stdioLocked(logWriter{c.logs, Stdout})
		spec.Stdin, spec.Terminal, endStdio = in, term, end
		if out != nil {
			spec.Stdout, spec.Stderr = out, errw
		}
		if c.cfg.Tty {
			spec.Env = appendIfMissing(spec.Env, "TERM=xterm")
		}
	}
	c.state = Running
	c.startedAt = time.Now().UTC()
	c.finishedAt = time.Time{}
	c.errMsg, c.exitCode, c.timedOut = "", 0, false
	c.notifyLocked()
	c.mu.Unlock()
	m.schedule(c)

	detach := func() {
		endStdio()
		if ports != nil {
			ports.Close() // the host ports are free again, as Docker frees them at stop
		}
		if ep != nil {
			ep.Detach() // its listeners go; the name keeps its address
		}
	}
	proc, err := m.opt.Executor.Start(ctx, spec)
	if err != nil {
		detach()
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
	pending := c.killPending
	c.killPending = false
	stopHealth := m.startHealthLocked(c)
	c.notifyLocked() // an exec waiting for the process (CreateExec)
	c.mu.Unlock()
	if pending {
		proc.Kill() // a kill arrived before the process existed
	}
	m.emit("start", c, nil)
	go m.checkpointWhileRunning(c)
	m.exits.Add(1)
	go func() {
		defer m.exits.Done()
		res := proc.Wait()
		stopHealth()
		detach()
		c.finish(res)
		m.exitedAfterRun(c)
	}()
	return nil
}

// exitedAfterRun is exited for a process that ran: its restart policy may
// start it again.
func (m *Manager) exitedAfterRun(c *Container) {
	c.mu.Lock()
	restart := m.shouldRestartLocked(c)
	code := c.exitCode
	if restart {
		m.scheduleRestartLocked(c)
	}
	c.mu.Unlock()
	if !restart {
		m.exited(c)
		return
	}
	m.emitDie(c, code)
	m.schedule(c)
}

// emitDie publishes an exit's "die" and then lets the waiters for
// not-running go: `stop` and `kill` callers see die first.
func (m *Manager) emitDie(c *Container, code int) {
	m.emit("die", c, map[string]string{"exitCode": strconv.Itoa(code)})
	c.mu.Lock()
	c.dies++
	c.notifyLocked()
	c.mu.Unlock()
}

// exitStartFailed is the exit code of a container that could not start
// (a volume, a network or a published port it could not have): Docker's
// 128 for the same.
const exitStartFailed = 128

// failStart records a start that failed before the process existed, the
// way a process that could not run is recorded: the container has exited
// with the error, a --rm container is removed, and every waiter is told.
// Returning the error alone left a `run --rm` waiting for an exit that was
// never going to come. The caller holds c.mu; failStart releases it.
func (m *Manager) failStart(c *Container, err error) error {
	c.mu.Unlock()
	c.finish(runtime.Result{ExitCode: exitStartFailed, Error: err.Error()})
	m.exited(c)
	return err
}

// exited stores an exit, or removes a --rm container.
func (m *Manager) exited(c *Container) {
	c.mu.Lock()
	code := c.exitCode
	c.mu.Unlock()
	m.emitDie(c, code)
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
	live := m.byID[c.id] == c
	if live {
		delete(m.byID, c.id)
		delete(m.byName, c.name)
	}
	m.mu.Unlock()
	if live {
		m.emit("destroy", c, nil)
	}
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
	if m.opt.Events != nil {
		m.opt.Events.Publish(events.Event{Type: events.Container, Action: "prune",
			Attributes: map[string]string{"reclaimed": strconv.FormatInt(bytes, 10)}})
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
	c.ip = netip.Addr{}
	c.net = nil
	c.ports = nil
	c.killPending = false
	c.exits++
	c.notifyLocked()
}

// Kill ends a running container.
func (m *Manager) Kill(ref string) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	if c.stopByUser() { // waiting for its policy's restart: it stays stopped
		m.schedule(c)
		return nil
	}
	if !c.kill() {
		return ErrNotRunning
	}
	m.emit("kill", c, map[string]string{"signal": "9"})
	return nil
}

// Stop ends a running container and waits until it has exited. A wasm
// guest has no signal to catch, so there is no grace period: stopping is
// killing (docker stop's SIGKILL after the timeout, at once).
func (m *Manager) Stop(ctx context.Context, ref string) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	if c.stopByUser() { // waiting for its policy's restart: it stays stopped
		m.schedule(c)
		m.emit("stop", c, nil)
		return nil
	}
	if !c.kill() {
		return ErrNotRunning
	}
	m.emit("kill", c, map[string]string{"signal": "9"})
	if _, err = c.Wait(ctx, WaitNotRunning); err != nil {
		return err
	}
	m.emit("stop", c, nil)
	return nil
}

// Restart stops the container if it runs, then starts it.
func (m *Manager) Restart(ctx context.Context, ref string) error {
	if err := m.Stop(ctx, ref); err != nil && !errors.Is(err, ErrNotRunning) {
		return err
	}
	if err := m.Start(ctx, ref); err != nil {
		return err
	}
	if c, err := m.Get(ref); err == nil {
		m.emit("restart", c, nil)
	}
	return nil
}

// Attach streams to fn what the container writes from now on (with logs,
// what it wrote before too) until its next exit — of the run in progress,
// or of the next start when it is not running — or its removal.
func (c *Container) Attach(ctx context.Context, logs bool, fn func(Record) error) error {
	c.mu.Lock()
	startExits := c.exits
	c.mu.Unlock()
	seq := 0
	if !logs {
		_, seq, _ = c.logs.since(0)
	}
	for {
		c.mu.Lock()
		done := c.exits > startExits || c.removed
		stateCh := c.changed
		c.mu.Unlock()
		recs, next, logCh := c.logs.since(seq)
		for _, r := range recs {
			if err := fn(r); err != nil {
				return err
			}
		}
		seq = next
		if done {
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

// kill ends c if it is running, and reports whether it was. Between Start
// marking it running and its process being attached there is no process
// to kill yet: the kill is then kept and delivered on attachment.
func (c *Container) kill() bool {
	c.mu.Lock()
	if c.state != Running {
		c.mu.Unlock()
		return false
	}
	p := c.proc
	if p == nil {
		c.killPending = true
	}
	c.mu.Unlock()
	if p != nil {
		p.Kill()
	}
	return true
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
			ok = c.state != Running && c.dies >= c.exits
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
	running := c.state == Running || c.state == Restarting
	c.mu.Unlock()
	if running {
		if !force {
			return fmt.Errorf("%w: stop the container before removing it or force remove", ErrRunning)
		}
		c.stopByUser()
		c.kill()
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
	m.stopping.Store(true) // no policy restarts anything from here on
	m.mu.Lock()
	cs := make([]*Container, 0, len(m.byID))
	for _, c := range m.byID {
		cs = append(cs, c)
	}
	m.mu.Unlock()
	for _, c := range cs {
		c.mu.Lock()
		if c.cancelRestartLocked() {
			m.schedule(c)
		}
		c.mu.Unlock()
	}
	for _, c := range cs {
		if c.kill() { // also one still being attached
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
