package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
)

// Exec instances (the Engine API's exec): a further process started in a
// running container, on its trees, network and capabilities and in its
// process group. An instance is created, then started once; its exit code
// stays readable until the container is removed or the table fills.

// Errors of exec.
var (
	ErrNoSuchExec     = errors.New("no such exec instance")
	ErrExecStarted    = errors.New("exec instance already started")
	ErrExecNotRunning = errors.New("exec instance is not running")
)

// MaxExecs bounds the exec instances kept per manager; the oldest ended
// ones go first.
const MaxExecs = 256

// ExecConfig is an exec instance as created.
type ExecConfig struct {
	Cmd        []string
	Env        []string
	WorkingDir string
	// User must be empty, "root" or "0": a WebAssembly container has no
	// users.
	User                                    string
	Tty                                     bool
	AttachStdin, AttachStdout, AttachStderr bool
	// ConsoleSize is the initial terminal size, height then width (0, 0:
	// 80x24).
	ConsoleSize [2]uint16
}

// Exec is one exec instance.
type Exec struct {
	ID          string
	ContainerID string
	Config      ExecConfig
	created     time.Time

	mu       sync.Mutex
	started  bool
	running  bool
	exitCode int
	pid      uint32
	proc     runtime.Process
	term     *wasi.Terminal
	done     chan struct{}
}

// ExecInfo is an instance's state (GET /exec/{id}/json).
type ExecInfo struct {
	ID, ContainerID string
	Config          ExecConfig
	Running         bool
	ExitCode        int
	// Started reports whether a start was asked; Ended, that it has
	// ended (or failed to start): ExitCode is meaningful then.
	Started, Ended bool
	Pid            uint32
}

type execTable struct {
	mu   sync.Mutex
	byID map[string]*Exec
}

// CreateExec prepares cmd to run in container ref, which must be running.
func (m *Manager) CreateExec(ref string, cfg ExecConfig) (*Exec, error) {
	if len(cfg.Cmd) == 0 || cfg.Cmd[0] == "" {
		return nil, fmt.Errorf("%w: no command", ErrInvalid)
	}
	switch cfg.User {
	case "", "root", "0", "0:0", "root:root":
	default:
		return nil, fmt.Errorf("%w: user %q: a WebAssembly container has no users", ErrInvalid, cfg.User)
	}
	for _, kv := range cfg.Env {
		if k, _, ok := strings.Cut(kv, "="); !ok || k == "" || strings.ContainsRune(kv, 0) {
			return nil, fmt.Errorf("%w: environment entry %q", ErrInvalid, kv)
		}
	}
	c, err := m.Get(ref)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	running := c.state == Running && c.proc != nil
	c.mu.Unlock()
	if !running {
		return nil, fmt.Errorf("%w: %s", ErrNotRunning, c.id[:12])
	}
	x := &Exec{ID: randomHex(32), ContainerID: c.id, Config: cfg, created: time.Now(), done: make(chan struct{})}
	t := &m.execs
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byID == nil {
		t.byID = map[string]*Exec{}
	}
	if len(t.byID) >= MaxExecs {
		t.evictLocked()
	}
	if len(t.byID) >= MaxExecs {
		return nil, fmt.Errorf("%w: %d exec instances are running", ErrTooMany, len(t.byID))
	}
	t.byID[x.ID] = x
	return x, nil
}

// evictLocked drops the oldest instances that are not running.
func (t *execTable) evictLocked() {
	var ended []*Exec
	for _, x := range t.byID {
		x.mu.Lock()
		if !x.running {
			ended = append(ended, x)
		}
		x.mu.Unlock()
	}
	sort.Slice(ended, func(i, j int) bool { return ended[i].created.Before(ended[j].created) })
	for _, x := range ended[:max(0, len(ended)-MaxExecs/2)] {
		delete(t.byID, x.ID)
	}
	if len(t.byID) >= MaxExecs && len(ended) > 0 {
		delete(t.byID, ended[len(ended)-1].ID)
	}
}

// GetExec finds an instance by its full ID or a unique prefix.
func (m *Manager) GetExec(id string) (*Exec, error) {
	t := &m.execs
	t.mu.Lock()
	defer t.mu.Unlock()
	if x, ok := t.byID[id]; ok {
		return x, nil
	}
	var found *Exec
	if len(id) >= 4 {
		for k, x := range t.byID {
			if strings.HasPrefix(k, id) {
				if found != nil {
					return nil, fmt.Errorf("%w: %s", ErrAmbiguous, id)
				}
				found = x
			}
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %s", ErrNoSuchExec, id)
	}
	return found, nil
}

// ExecIO is where a started instance reads and writes. With a terminal,
// Stderr is not used: the terminal carries both outputs, as Docker does.
type ExecIO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
}

// StartExec runs instance x. ctx bounds the run: cancelling it kills the
// process (an attached client that hangs up), so a detached start passes
// a context that is never cancelled.
func (m *Manager) StartExec(ctx context.Context, x *Exec, sio ExecIO) error {
	x.mu.Lock()
	if x.started {
		x.mu.Unlock()
		return fmt.Errorf("%w: %s", ErrExecStarted, x.ID[:12])
	}
	x.started = true
	x.mu.Unlock()
	fail := func(err error, code int) error {
		x.mu.Lock()
		x.exitCode = code
		x.mu.Unlock()
		close(x.done)
		return err
	}
	c, err := m.Get(x.ContainerID)
	if err != nil {
		return fail(err, runtime.ExitMissing)
	}
	c.mu.Lock()
	proc, running := c.proc, c.state == Running
	env := append([]string{}, c.cfg.Env...)
	workdir := c.cfg.WorkingDir
	hostname := c.id[:12]
	fs := c.fs
	c.mu.Unlock()
	execer, ok := proc.(runtime.Execer)
	if !running || proc == nil {
		return fail(fmt.Errorf("%w: %s", ErrNotRunning, hostname), runtime.ExitMissing)
	}
	if !ok {
		return fail(runtime.ErrNoExec, runtime.ExitNoExec)
	}
	cfg := x.Config
	env = append(env, cfg.Env...)
	if _, ok := envValue(env, "PATH"); !ok {
		env = append([]string{"PATH=" + defaultPath}, env...)
	}
	if _, ok := envValue(env, "HOSTNAME"); !ok {
		env = append(env, "HOSTNAME="+hostname)
	}
	if cfg.WorkingDir != "" {
		workdir = cfg.WorkingDir
	}
	workdir = path.Clean("/" + workdir)
	env = append(env, "PWD="+workdir)
	if cfg.Tty {
		env = appendIfMissing(env, "TERM=xterm")
	}
	modPath := resolveModule(fs, cfg.Cmd[0], workdir, env)

	spec := runtime.ExecSpec{Path: modPath, Args: cfg.Cmd, Env: env}
	if cfg.AttachStdout {
		spec.Stdout = sio.Stdout
	}
	if cfg.AttachStderr {
		spec.Stderr = sio.Stderr
	}
	var feed func()
	if cfg.Tty {
		h, w := cfg.ConsoleSize[0], cfg.ConsoleSize[1]
		if h == 0 || w == 0 {
			h, w = 24, 80
		}
		x.term = wasi.NewTerminal(w, h, [3]bool{cfg.AttachStdin, cfg.AttachStdout, cfg.AttachStdout})
		var out io.Writer = io.Discard
		if cfg.AttachStdout && sio.Stdout != nil {
			out = sio.Stdout
		}
		cr := &crlf{w: out}
		spec.Stdout, spec.Stderr, spec.Terminal = cr, cr, x.term
		if cfg.AttachStdin && sio.Stdin != nil {
			pr, pw := io.Pipe()
			l := &ldisc{term: x.term, pw: pw, echo: rawEcho{cr}}
			spec.Stdin = pr
			feed = func() { l.run(sio.Stdin) }
		}
	} else if cfg.AttachStdin && sio.Stdin != nil {
		spec.Stdin = sio.Stdin
	}
	p, pid, err := execer.Exec(ctx, spec)
	if err != nil {
		code := runtime.ExitNoExec
		if errors.Is(err, runtime.ErrNotFound) {
			code = runtime.ExitMissing
		}
		return fail(err, code)
	}
	x.mu.Lock()
	x.running, x.pid, x.proc = true, pid, p
	x.mu.Unlock()
	if feed != nil {
		go feed()
	}
	m.emitExec("exec_start", c, x)
	go func() {
		res := p.Wait()
		x.mu.Lock()
		x.running, x.exitCode = false, res.ExitCode
		x.mu.Unlock()
		close(x.done)
		m.emitExec("exec_die", c, x)
	}()
	return nil
}

func appendIfMissing(env []string, kv string) []string {
	k, _, _ := strings.Cut(kv, "=")
	if _, ok := envValue(env, k); ok {
		return env
	}
	return append(env, kv)
}

func (m *Manager) emitExec(action string, c *Container, x *Exec) {
	m.emit(action+": "+strings.Join(x.Config.Cmd, " "), c, map[string]string{"execID": x.ID})
}

// Done is closed when the instance has ended (or failed to start).
func (x *Exec) Done() <-chan struct{} { return x.done }

// Info is the instance's state.
func (x *Exec) Info() ExecInfo {
	ended := false
	select {
	case <-x.done:
		ended = true
	default:
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	return ExecInfo{ID: x.ID, ContainerID: x.ContainerID, Config: x.Config, Running: x.running,
		ExitCode: x.exitCode, Started: x.started, Ended: ended, Pid: x.pid}
}

// Resize changes the terminal size of a running exec -t.
func (x *Exec) Resize(h, w uint16) error {
	x.mu.Lock()
	t, running := x.term, x.running
	x.mu.Unlock()
	if t == nil || !running {
		return fmt.Errorf("%w: %s has no running terminal", ErrExecNotRunning, x.ID[:12])
	}
	if h == 0 || w == 0 {
		return fmt.Errorf("%w: size %dx%d", ErrInvalid, w, h)
	}
	t.SetSize(w, h)
	return nil
}

// Kill stops a running instance.
func (x *Exec) Kill() {
	x.mu.Lock()
	p := x.proc
	x.mu.Unlock()
	if p != nil {
		p.Kill()
	}
}
