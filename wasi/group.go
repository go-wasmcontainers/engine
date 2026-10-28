package wasi

import (
	"strings"
	"sync"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
)

// Processes: a container's processes form a Group (one per container).
// The group numbers them (pid 1 is the container's first process), bounds
// how many run, how much linear memory they may reserve together and how
// fast they are spawned, and kills them all at once. A process spawns a
// child through its executor's Spawner, from a descriptor table made by
// CloneFDs; the parent keeps a Child to wait on.
//
// These are the primitives; the WASIX calls (proc_spawn2, proc_spawn3,
// proc_join, fd_pipe, the tty calls) are a host module on top.

// Errno values the process primitives return, beyond those of wasi.go.
const (
	ECHILD  = 12
	ENOEXEC = 45
	ENOMEM  = 48
	ENOTTY  = 59
	ESRCH   = 71
)

// GroupLimits bound a container's processes. Zero values take the
// defaults.
type GroupLimits struct {
	// MaxProcs bounds the processes alive at once (default 64).
	MaxProcs int
	// MaxMemoryBytes bounds the sum of their memory limits (default: no
	// bound beyond MaxProcs).
	MaxMemoryBytes int64
	// SpawnRate and SpawnBurst bound spawns per second, as a token bucket
	// (defaults 32 per second, bursts of 64).
	SpawnRate  float64
	SpawnBurst int
}

// Group is a container's process tree.
type Group struct {
	lim GroupLimits

	mu      sync.Mutex
	next    uint32
	procs   map[uint32]*Proc
	kills   map[uint32]func()
	mem     int64
	pending int // admitted, not yet registered
	tokens  float64
	last    time.Time
	dead    bool
}

// NewGroup returns an empty group.
func NewGroup(lim GroupLimits) *Group {
	if lim.MaxProcs <= 0 {
		lim.MaxProcs = 64
	}
	if lim.SpawnRate <= 0 {
		lim.SpawnRate = 32
	}
	if lim.SpawnBurst <= 0 {
		lim.SpawnBurst = 64
	}
	return &Group{lim: lim, next: 1, procs: map[uint32]*Proc{}, kills: map[uint32]func(){},
		tokens: float64(lim.SpawnBurst), last: time.Now()}
}

// Admit reserves room for one more process of memory limit mem: EAGAIN
// past the process count or the spawn rate, ENOMEM past the memory
// budget, ESRCH once the group was killed. A successful Admit is undone
// by the process's Close, or by Cancel if it never starts.
func (g *Group) Admit(mem int64) uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.dead {
		return ESRCH
	}
	now := time.Now()
	g.tokens = min(float64(g.lim.SpawnBurst), g.tokens+now.Sub(g.last).Seconds()*g.lim.SpawnRate)
	g.last = now
	if len(g.procs)+g.pending >= g.lim.MaxProcs || g.tokens < 1 {
		return EAGAIN
	}
	if g.lim.MaxMemoryBytes > 0 && g.mem+mem > g.lim.MaxMemoryBytes {
		return ENOMEM
	}
	g.tokens--
	g.mem += mem
	g.pending++
	return 0
}

// Cancel undoes an Admit whose process never started.
func (g *Group) Cancel(mem int64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.mem -= mem
	g.pending--
}

// register numbers p; admitted says an Admit reserved its room.
func (g *Group) register(p *Proc, admitted bool) uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if admitted {
		g.pending--
	} else {
		g.mem += p.cfg.MemoryLimitBytes
	}
	pid := g.next
	g.next++
	g.procs[pid] = p
	return pid
}

func (g *Group) unregister(p *Proc) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.procs[p.pid] == p {
		delete(g.procs, p.pid)
		delete(g.kills, p.pid)
		g.mem -= p.cfg.MemoryLimitBytes
	}
}

// OnKill registers how to stop process pid (its executor's kill).
func (g *Group) OnKill(pid uint32, kill func()) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, ok := g.procs[pid]; ok {
		g.kills[pid] = kill
	}
}

// Kill stops every process of the group and refuses new ones.
func (g *Group) Kill() {
	g.mu.Lock()
	g.dead = true
	kills := make([]func(), 0, len(g.kills))
	for _, k := range g.kills {
		kills = append(kills, k)
	}
	g.mu.Unlock()
	for _, k := range kills {
		k()
	}
}

// Len is the number of live processes.
func (g *Group) Len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.procs)
}

// Pids lists the live processes' pids and parents' pids (0 for none).
func (g *Group) Pids() map[uint32]uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[uint32]uint32, len(g.procs))
	for pid, p := range g.procs {
		var ppid uint32
		if p.cfg.Parent != nil {
			ppid = p.cfg.Parent.pid
		}
		out[pid] = ppid
	}
	return out
}

// Pid is this process's number in its container (1 for the first).
func (p *Proc) Pid() uint32 { return p.pid }

// Group is the process's container group.
func (p *Proc) Group() *Group { return p.cfg.Group }

// ExitStatus is how a process ended.
type ExitStatus struct {
	// Code is the proc_exit code (the full u32; WASIX reports its low 16
	// bits, so 0..255 survive), when Signal is 0.
	Code uint32
	// Signal is 9 (SIGKILL) for a process killed or timed out, 6
	// (SIGABRT) for one that trapped, 0 for a normal exit.
	Signal int
}

// Child is a process started by Spawn, as its parent sees it.
type Child struct {
	pid    uint32
	done   chan struct{}
	status ExitStatus
	kill   func()
	once   sync.Once
}

// NewChild is for executors: the handle Spawn returns, with the child's
// pid and how to kill it. The executor calls Exit when it ends.
func NewChild(pid uint32, kill func()) *Child {
	return &Child{pid: pid, done: make(chan struct{}), kill: kill}
}

// Pid is the child's number.
func (c *Child) Pid() uint32 { return c.pid }

// Done is closed when the child has ended.
func (c *Child) Done() <-chan struct{} { return c.done }

// Status is how the child ended; valid once Done is closed.
func (c *Child) Status() ExitStatus { return c.status }

// Kill stops the child.
func (c *Child) Kill() {
	if c.kill != nil {
		c.kill()
	}
}

// Exit records the child's end (executors call it once).
func (c *Child) Exit(s ExitStatus, parent *Proc) {
	c.once.Do(func() {
		c.status = s
		close(c.done)
		if parent != nil {
			parent.childExited()
		}
	})
}

// SpawnSpec is a child to start.
type SpawnSpec struct {
	// Path is the module's absolute guest path in the container.
	Path string
	// Argv is argv, argv[0] included (it need not be Path).
	Argv []string
	// Env is "KEY=value" entries.
	Env []string
	// FDs is the child's table and cwd (CloneFDs); Spawn takes it over,
	// and releases it on failure.
	FDs *FDTable
}

// MaxArgBytes bounds a spawn's argv and env together (Linux's ARG_MAX
// order of magnitude): E2BIG past it.
const MaxArgBytes = 256 << 10

// CheckArgs is E2BIG when argv and env exceed MaxArgBytes.
func (s SpawnSpec) CheckArgs() uint32 {
	n := 0
	for _, a := range s.Argv {
		n += len(a) + 1
	}
	for _, e := range s.Env {
		n += len(e) + 1
	}
	if n > MaxArgBytes || len(s.Argv)+len(s.Env) > 1<<14 {
		return E2BIG
	}
	return 0
}

// Spawner starts child processes. Spawn returns ENOENT (nothing at
// Path), ENOEXEC (not a WebAssembly module), EAGAIN or ENOMEM (a group
// limit), E2BIG (CheckArgs), ESRCH (the group was killed); the child is
// already running when it returns.
type Spawner interface {
	Spawn(parent *Proc, spec SpawnSpec) (*Child, uint32)
}

// Spawn starts a child with the process's Spawner and records it.
func (p *Proc) Spawn(spec SpawnSpec) (*Child, uint32) {
	if p.cfg.Spawner == nil {
		if spec.FDs != nil {
			spec.FDs.Close(p)
		}
		return nil, ENOTSUP
	}
	if e := spec.CheckArgs(); e != 0 {
		if spec.FDs != nil {
			spec.FDs.Close(p)
		}
		return nil, e
	}
	c, e := p.cfg.Spawner.Spawn(p, spec)
	if e != 0 {
		return nil, e
	}
	p.childMu.Lock()
	p.children = append(p.children, c)
	p.childMu.Unlock()
	return c, 0
}

// SpawnWait is Spawn for a host call: the spawn (which may compile the
// module, and in the browser waits on the event loop) runs off the call,
// and the call resumes with then once the child runs or the spawn failed.
// A host function returns its result; it never blocks the engine.
func (p *Proc) SpawnWait(spec SpawnSpec, then func(c *Child, errno uint32, m Memory) Result) Result {
	done := make(chan struct{})
	var c *Child
	var e uint32
	go func() {
		c, e = p.Spawn(spec)
		close(done)
	}()
	return Result{Wait: done, Resume: func(m Memory) Result { return then(c, e, m) }}
}

func (p *Proc) childExited() {
	p.childMu.Lock()
	defer p.childMu.Unlock()
	close(p.childWake)
	p.childWake = make(chan struct{})
}

// Children lists the children not yet reaped.
func (p *Proc) Children() []*Child {
	p.childMu.Lock()
	defer p.childMu.Unlock()
	return append([]*Child(nil), p.children...)
}

// ChildChanged returns a channel closed at the next child exit (take it
// before scanning Children, so an exit in between is not missed).
func (p *Proc) ChildChanged() <-chan struct{} {
	p.childMu.Lock()
	defer p.childMu.Unlock()
	return p.childWake
}

// Reap removes an ended child from Children; pid 0 reaps any ended one.
// It returns the child, nil with ECHILD when there is no such child, or
// nil with EAGAIN when none of them has ended yet.
func (p *Proc) Reap(pid uint32) (*Child, uint32) {
	p.childMu.Lock()
	defer p.childMu.Unlock()
	found := false
	for i, c := range p.children {
		if pid != 0 && c.pid != pid {
			continue
		}
		found = true
		select {
		case <-c.done:
			p.children = append(p.children[:i], p.children[i+1:]...)
			return c, 0
		default:
		}
	}
	if !found {
		return nil, ECHILD
	}
	return nil, EAGAIN
}

// Terminal is a container's terminal (run -t, exec -it).
type Terminal struct {
	mu         sync.Mutex
	cols, rows uint16
	// TTY says which of fds 0, 1, 2 are the terminal.
	TTY [3]bool
	// Echo and Line are the line discipline's echo and canonical mode.
	echo, line bool
}

// NewTerminal returns a terminal of the given size, echoing, in line mode.
func NewTerminal(cols, rows uint16, tty [3]bool) *Terminal {
	return &Terminal{cols: cols, rows: rows, TTY: tty, echo: true, line: true}
}

// Size and SetSize read and change the window size.
func (t *Terminal) Size() (cols, rows uint16) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cols, t.rows
}

// SetSize changes the window size (a resize from the front end).
func (t *Terminal) SetSize(cols, rows uint16) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cols, t.rows = cols, rows
}

// Mode and SetMode read and change echo and line mode.
func (t *Terminal) Mode() (echo, line bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.echo, t.line
}

// SetMode sets echo and line mode.
func (t *Terminal) SetMode(echo, line bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.echo, t.line = echo, line
}

// Terminal is the process's terminal, nil without one.
func (p *Proc) Terminal() *Terminal { return p.cfg.Terminal }

// IsTerminal reports whether fd is the terminal: one of fds 0..2 marked
// TTY and still the descriptor the process started with there.
func (p *Proc) IsTerminal(fd uint32) bool {
	t := p.cfg.Terminal
	f := p.fds[fd]
	if t == nil || f == nil {
		return false
	}
	switch f.kind {
	case kindStdin:
		return t.TTY[0]
	case kindStdout:
		return t.TTY[1]
	case kindStderr:
		return t.TTY[2]
	}
	return false
}

// Stat looks an absolute guest path up in the container's trees (the
// preopens), without following a final symlink when nofollow.
func (p *Proc) Stat(abs string, nofollow bool) (*rootfs.Info, uint32) {
	if !strings.HasPrefix(abs, "/") {
		return nil, EINVAL
	}
	pre, rel := p.preopenFor(&FDTable{fds: p.fds}, resolve("/", abs))
	if pre == nil {
		return nil, ENOENT
	}
	var in *rootfs.Info
	var err error
	if nofollow {
		in, err = pre.fs.Lstat(rel)
	} else {
		in, err = pre.fs.Stat(rel)
	}
	if err != nil {
		return nil, errno(err)
	}
	return in, 0
}

// LookPath finds a command: name itself when it contains a "/" (against
// cwd), else each directory of pathList (colon-separated) as
// "<dir>/<name>" then "<dir>/<name>.wasm". It returns the first regular
// file, or ENOENT.
func (p *Proc) LookPath(name, pathList, cwd string) (string, uint32) {
	if name == "" {
		return "", ENOENT
	}
	try := func(abs string) bool {
		in, e := p.Stat(abs, false)
		return e == 0 && in.Mode().IsRegular()
	}
	if strings.Contains(name, "/") {
		abs := resolve(cwd, name)
		if try(abs) {
			return abs, 0
		}
		return "", ENOENT
	}
	for _, dir := range strings.Split(pathList, ":") {
		if dir == "" {
			dir = "."
		}
		base := resolve(cwd, dir) + "/" + name
		for _, c := range []string{base, base + ".wasm"} {
			if try(c) {
				return resolve("/", c), 0
			}
		}
	}
	return "", ENOENT
}
