// Package wazero is the native Executor: it runs a container's wasip1
// module with wazero (pure Go, no cgo), the container's rootfs mounted as
// the guest's "/".
package wazero

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/experimental/sysfs"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/runtime"
	"github.com/go-wasmcontainers/engine/wasi"
)

const pageSize = 65536

// Options configures the executor.
type Options struct {
	// DefaultMemoryLimitBytes applies when a Spec sets none (default 256 MiB).
	DefaultMemoryLimitBytes int64
	// MaxCompiled bounds the compiled-module cache (default 64 modules).
	MaxCompiled int
	// Interpreter forces wazero's interpreter instead of its compiler.
	Interpreter bool
	// EngineWASI serves wasi_snapshot_preview1 with the engine's own host
	// (package wasi, the one the browser executor uses) instead of
	// wazero's. It exists so that host is exercised natively by real
	// guests.
	EngineWASI bool
	// HostModules are served beside wasi_snapshot_preview1 (wasi-nn,
	// wasi:webgpu, …). They run in the engine's WASI host, so setting any
	// implies EngineWASI.
	HostModules []wasi.HostModule
}

// Executor runs modules with wazero. Compiled modules are cached by the
// sha256 of their bytes and memory limit, so a second container of the
// same image does not compile again.
type Executor struct {
	opt   Options
	cache wazero.CompilationCache

	mu       sync.Mutex
	runtimes map[uint32]wazero.Runtime // by memory limit in pages
	compiled map[cacheKey]*cacheEntry
	tick     uint64
	closed   bool
}

type cacheKey struct {
	pages  uint32
	digest image.Digest
}

type cacheEntry struct {
	mod  wazero.CompiledModule
	used uint64
}

// New returns an executor.
func New(opt Options) *Executor {
	if opt.DefaultMemoryLimitBytes <= 0 {
		opt.DefaultMemoryLimitBytes = 256 << 20
	}
	if opt.MaxCompiled <= 0 {
		opt.MaxCompiled = 64
	}
	if len(opt.HostModules) > 0 {
		opt.EngineWASI = true
	}
	return &Executor{
		opt:      opt,
		cache:    wazero.NewCompilationCache(),
		runtimes: map[uint32]wazero.Runtime{},
		compiled: map[cacheKey]*cacheEntry{},
	}
}

var _ runtime.Executor = (*Executor)(nil)

func pagesFor(bytes int64) uint32 {
	p := bytes / pageSize
	switch {
	case p < 1:
		return 1
	case p > 65536:
		return 65536
	}
	return uint32(p)
}

// runtimeLocked returns the runtime for a memory limit, creating it (and
// instantiating WASI in it) on first use.
func (e *Executor) runtimeLocked(ctx context.Context, pages uint32) (wazero.Runtime, error) {
	if r, ok := e.runtimes[pages]; ok {
		return r, nil
	}
	cfg := wazero.NewRuntimeConfig()
	if e.opt.Interpreter {
		cfg = wazero.NewRuntimeConfigInterpreter()
	}
	cfg = cfg.WithCompilationCache(e.cache).
		WithMemoryLimitPages(pages).
		WithCloseOnContextDone(true).
		// Exception handling: C and C++ guests compiled with wasm
		// exceptions (lua's error()) need it; browsers ship it (Chrome 95,
		// Firefox 100). wazero has it behind an experimental flag, in its
		// interpreter and its compiler.
		WithCoreFeatures(api.CoreFeaturesV2 | experimental.CoreFeaturesExceptionHandling)
	r := wazero.NewRuntimeWithConfig(ctx, cfg)
	var err error
	if e.opt.EngineWASI {
		_, err = instantiateEngineWASI(ctx, r, e.opt.HostModules)
	} else {
		_, err = wasi_snapshot_preview1.Instantiate(ctx, r)
	}
	if err != nil {
		r.Close(ctx)
		return nil, err
	}
	e.runtimes[pages] = r
	return r, nil
}

// compile returns the compiled module for wasm under a memory limit.
func (e *Executor) compile(ctx context.Context, pages uint32, wasm []byte) (wazero.Runtime, wazero.CompiledModule, error) {
	key := cacheKey{pages: pages, digest: image.FromBytes(wasm)}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil, nil, errors.New("executor closed")
	}
	r, err := e.runtimeLocked(ctx, pages)
	if err != nil {
		e.mu.Unlock()
		return nil, nil, err
	}
	e.tick++
	if c, ok := e.compiled[key]; ok {
		c.used = e.tick
		e.mu.Unlock()
		return r, c.mod, nil
	}
	e.mu.Unlock()
	// Compile outside the lock: it takes long, and two containers of
	// different images must not wait for each other. Two first starts of
	// the same image may both compile; the second result is dropped.
	mod, err := r.CompileModule(ctx, wasm)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", runtime.ErrInvalid, err)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.compiled[key]; ok {
		mod.Close(ctx)
		return r, c.mod, nil
	}
	if len(e.compiled) >= e.opt.MaxCompiled {
		var oldK cacheKey
		var old *cacheEntry
		for k, c := range e.compiled {
			if old == nil || c.used < old.used {
				oldK, old = k, c
			}
		}
		// A running instance keeps its own reference; closing the compiled
		// module only drops the cache's.
		old.mod.Close(ctx)
		delete(e.compiled, oldK)
	}
	e.compiled[key] = &cacheEntry{mod: mod, used: e.tick}
	return r, mod, nil
}

// CacheLen reports how many compiled modules are cached.
func (e *Executor) CacheLen() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.compiled)
}

// Close implements runtime.Executor.
func (e *Executor) Close(ctx context.Context) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	var errs []error
	for _, r := range e.runtimes {
		errs = append(errs, r.Close(ctx))
	}
	errs = append(errs, e.cache.Close(ctx))
	return errors.Join(errs...)
}

type process struct {
	done   chan struct{}
	res    runtime.Result
	cancel context.CancelFunc
	mu     sync.Mutex
	killed bool
	mem    atomic.Int64 // linear memory bytes, published by measuredMemory
	limit  int64
}

// Stats implements runtime.StatsReporter.
func (p *process) Stats() runtime.Stats {
	return runtime.Stats{MemoryBytes: p.mem.Load(), MemoryLimitBytes: p.limit}
}

func (p *process) Wait() runtime.Result { <-p.done; return p.res }

func (p *process) Kill() {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	p.cancel()
}

// Start implements runtime.Executor.
func (e *Executor) Start(ctx context.Context, spec runtime.Spec) (runtime.Process, error) {
	wasm, err := runtime.LoadModule(spec)
	if err != nil {
		return nil, err
	}
	limit := spec.MemoryLimitBytes
	if limit <= 0 {
		limit = e.opt.DefaultMemoryLimitBytes
	}
	r, compiled, err := e.compile(ctx, pagesFor(limit), wasm)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", spec.Path, err)
	}

	args := spec.Args
	if len(args) == 0 {
		args = []string{spec.Path}
	}
	cfg := wazero.NewModuleConfig().
		WithName("").
		WithArgs(args...).
		WithSysWalltime().
		WithSysNanotime().
		WithSysNanosleep().
		WithRandSource(rand.Reader).
		WithFSConfig(fsConfig(spec))
	for _, kv := range spec.Env {
		k, v, _ := strings.Cut(kv, "=")
		if k == "" {
			continue
		}
		cfg = cfg.WithEnv(k, v)
	}
	stdin := spec.Stdin
	if stdin == nil {
		stdin = bytes.NewReader(nil)
	}
	stdout, stderr := spec.Stdout, spec.Stderr
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	cfg = cfg.WithStdin(stdin).WithStdout(stdout).WithStderr(stderr)

	runCtx, cancel := context.WithCancel(context.Background())
	var timedOut func() bool = func() bool { return false }
	if spec.Timeout > 0 {
		var tcancel context.CancelFunc
		runCtx, tcancel = context.WithTimeout(runCtx, spec.Timeout)
		inner := cancel
		cancel = func() { tcancel(); inner() }
		deadlineCtx := runCtx
		timedOut = func() bool { return errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) }
	}
	var proc *wasi.Proc
	if e.opt.EngineWASI {
		proc = wasi.New(wasi.Config{
			Args: args, Env: spec.Env, FS: spec.FS, Mounts: WASIMounts(spec.Mounts), Stdin: stdin, Stdout: stdout, Stderr: stderr,
			Net: spec.Net, Modules: e.opt.HostModules, Caps: spec.Caps,
		})
		runCtx = withProc(runCtx, proc)
	}
	p := &process{done: make(chan struct{}), cancel: cancel, limit: int64(pagesFor(limit)) * 65536}
	runCtx = experimental.WithMemoryAllocator(runCtx, allocator(&p.mem))
	// The caller's context bounds the start, and cancelling it kills the run.
	stop := context.AfterFunc(ctx, p.Kill)
	go func() {
		defer close(p.done)
		defer stop()
		defer cancel()
		mod, err := r.InstantiateModule(runCtx, compiled, cfg)
		if mod != nil {
			mod.Close(context.Background())
		}
		if proc != nil {
			proc.Close() // its sockets
		}
		p.mu.Lock()
		killed := p.killed
		p.mu.Unlock()
		p.res = result(err, killed, timedOut())
	}()
	return p, nil
}

// fsConfig preopens the root and each mount (wazero orders preopens as
// added: "/" first, then the volumes).
func fsConfig(spec runtime.Spec) wazero.FSConfig {
	c := wazero.NewFSConfig().(sysfs.FSConfig).WithSysFSMount(&sysFS{fs: spec.FS}, "/")
	for _, m := range spec.Mounts {
		c = c.(sysfs.FSConfig).WithSysFSMount(&sysFS{fs: m.FS, ro: m.ReadOnly}, m.Path)
	}
	return c
}

// WASIMounts converts runtime mounts for the engine's WASI host.
func WASIMounts(ms []runtime.Mount) []wasi.Mount {
	out := make([]wasi.Mount, len(ms))
	for i, m := range ms {
		out[i] = wasi.Mount{Path: m.Path, FS: m.FS, ReadOnly: m.ReadOnly}
	}
	return out
}

func result(err error, killed, timedOut bool) runtime.Result {
	switch {
	case timedOut:
		return runtime.Result{ExitCode: runtime.ExitKilled, TimedOut: true, Error: "wall-clock timeout"}
	case killed:
		return runtime.Result{ExitCode: runtime.ExitKilled, Killed: true, Error: "killed"}
	case err == nil:
		return runtime.Result{}
	}
	var ee *sys.ExitError
	if errors.As(err, &ee) {
		return runtime.Result{ExitCode: int(ee.ExitCode())}
	}
	return runtime.Result{ExitCode: runtime.ExitTrap, Error: err.Error()}
}
