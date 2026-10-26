package wazero

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"sync"
	"testing"
	"time"

	experimentalsys "github.com/tetratelabs/wazero/experimental/sys"

	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/runtime"
)

// syncBuf is a goroutine-safe buffer: the guest writes from its goroutine.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// lower builds a frozen image tree holding the named test programs.
func lower(t testing.TB, progs ...string) *rootfs.FS {
	f := rootfs.New()
	if err := f.MkdirAll("/bin", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.MkdirAll("/etc", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := f.WriteFile("/etc/motd", []byte("image\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range progs {
		if err := f.WriteFile("/bin/"+p+".wasm", testutil.Wasm(t, p), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	f.Freeze()
	return f
}

type run struct {
	res            runtime.Result
	stdout, stderr string
}

func start(t *testing.T, e *Executor, spec runtime.Spec) run {
	t.Helper()
	var out, errb syncBuf
	spec.Stdout, spec.Stderr = &out, &errb
	p, err := e.Start(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	res := p.Wait()
	return run{res, out.String(), errb.String()}
}

func TestRunPrograms(t *testing.T) {
	img := lower(t, "hello", "greet", "probe")
	e := New(opts())
	defer e.Close(context.Background())
	fsys := img.Clone(rootfs.Quota{})

	r := start(t, e, runtime.Spec{FS: fsys, Path: "/bin/hello.wasm"})
	if r.res.ExitCode != 0 || r.stdout != "Hello from a WebAssembly container!\n" {
		t.Fatalf("%+v", r)
	}
	r = start(t, e, runtime.Spec{FS: fsys, Path: "/bin/greet.wasm", Env: []string{"NAME=lab", "=ignored"}})
	if r.stdout != "Hello, lab!\n" {
		t.Fatalf("%q", r.stdout)
	}
	r = start(t, e, runtime.Spec{FS: fsys, Path: "/bin/probe.wasm", Args: []string{"probe", "exit", "7"}})
	if r.res.ExitCode != 7 || r.stderr != "bye\n" || r.res.Error != "" {
		t.Fatalf("%+v", r)
	}
	r = start(t, e, runtime.Spec{FS: fsys, Path: "/bin/probe.wasm", Args: []string{"probe", "args", "a b", "c"}})
	if r.stdout != "a b\nc\n" {
		t.Fatalf("%q", r.stdout)
	}
	r = start(t, e, runtime.Spec{FS: fsys, Path: "/bin/probe.wasm", Args: []string{"probe", "cwd"}, Env: []string{"PWD=/etc"}})
	if r.stdout != "/etc\n" {
		t.Fatalf("%q", r.stdout)
	}
	r = start(t, e, runtime.Spec{FS: fsys, Path: "/bin/probe.wasm", Args: []string{"probe", "cat", "motd"}, Env: []string{"PWD=/etc"}})
	if r.stdout != "image\n" {
		t.Fatalf("relative path from PWD: %q %q", r.stdout, r.stderr)
	}
	r = start(t, e, runtime.Spec{FS: fsys, Path: "/bin/probe.wasm", Args: []string{"probe", "ls", "/"}})
	if r.stdout != "bin\netc\n" {
		t.Fatalf("%q", r.stdout)
	}
	// Without Args, argv[0] is the path.
	r = start(t, e, runtime.Spec{FS: fsys, Path: "/bin/probe.wasm"})
	if r.res.ExitCode != 2 || !strings.Contains(r.stderr, "usage") {
		t.Fatalf("%+v", r)
	}
	if e.CacheLen() != 3 {
		t.Fatalf("cache holds %d modules, want 3", e.CacheLen())
	}
}

func TestFilesystemThroughWASI(t *testing.T) {
	img := lower(t, "probe")
	e := New(opts())
	defer e.Close(context.Background())
	fsys := img.Clone(rootfs.Quota{})
	r := start(t, e, runtime.Spec{FS: fsys, Path: "/bin/probe.wasm", Args: []string{"probe", "fs"}})
	want := "read: alpha\nappended: alpha+beta\nlink: sub/b.txt\nvia link: alpha+beta\nsize: 10\nleft: 0\nmotd: changed\n"
	if r.res.ExitCode != 0 || r.stdout != want {
		t.Fatalf("exit %d\nstdout %q\nstderr %q", r.res.ExitCode, r.stdout, r.stderr)
	}
	if b, _ := img.ReadFile("/etc/motd"); string(b) != "image\n" {
		t.Fatal("the container's write reached the image")
	}
	if b, _ := fsys.ReadFile("/etc/motd"); string(b) != "changed\n" {
		t.Fatal("the container lost its write")
	}
	// A read-only container root makes writes fail inside the guest.
	r = start(t, e, runtime.Spec{FS: img, Path: "/bin/probe.wasm", Args: []string{"probe", "fs"}})
	if r.res.ExitCode != 1 || !strings.Contains(r.stderr, "Read-only file system") {
		t.Fatalf("%+v", r)
	}
	// A quota-bound container gets an I/O error, not the host's memory.
	small := img.Clone(rootfs.Quota{MaxBytes: 2})
	r = start(t, e, runtime.Spec{FS: small, Path: "/bin/probe.wasm", Args: []string{"probe", "fs"}})
	if r.res.ExitCode != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestLimits(t *testing.T) {
	img := lower(t, "probe")
	e := New(opts())
	defer e.Close(context.Background())
	fsys := img.Clone(rootfs.Quota{})

	sp, err := e.Start(context.Background(), runtime.Spec{FS: fsys, Path: "/bin/probe.wasm", Args: []string{"probe", "spin"}, Timeout: 300 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now() // after the compile, which Start does synchronously
	if res := sp.Wait(); !res.TimedOut || res.ExitCode != runtime.ExitKilled {
		t.Fatalf("%+v", res)
	}
	if d := time.Since(t0); d > 10*time.Second {
		t.Fatalf("timeout took %v", d)
	}
	var r run

	// Memory: a 32 MiB bound stops a hog without touching the host.
	r = start(t, e, runtime.Spec{FS: fsys, Path: "/bin/probe.wasm", Args: []string{"probe", "hog"}, MemoryLimitBytes: 32 << 20})
	if r.res.ExitCode == 0 {
		t.Fatalf("hog exited 0: %+v", r)
	}
	t.Logf("memory hog under 32 MiB: exit %d, %q", r.res.ExitCode, firstLine(r.stderr))

	// Kill.
	p, err := e.Start(context.Background(), runtime.Spec{FS: fsys, Path: "/bin/probe.wasm", Args: []string{"probe", "spin"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	p.Kill()
	if res := p.Wait(); !res.Killed || res.ExitCode != runtime.ExitKilled {
		t.Fatalf("%+v", res)
	}
	// Cancelling the start context kills too.
	ctx, cancel := context.WithCancel(context.Background())
	p, err = e.Start(ctx, runtime.Spec{FS: fsys, Path: "/bin/probe.wasm", Args: []string{"probe", "spin"}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if res := p.Wait(); !res.Killed {
		t.Fatalf("%+v", res)
	}
}

func firstLine(s string) string { l, _, _ := strings.Cut(s, "\n"); return l }

func TestStartErrors(t *testing.T) {
	f := rootfs.New()
	f.MkdirAll("/bin", 0o755)
	f.WriteFile("/bin/text", []byte("#!/bin/sh\n"), 0o755)
	f.WriteFile("/bin/bad.wasm", []byte("\x00asm\x01\x00\x00\x00\xff\xff"), 0o755)
	e := New(Options{MaxCompiled: 1, Interpreter: true})
	ctx := context.Background()
	if _, err := e.Start(ctx, runtime.Spec{Path: "/x"}); err == nil {
		t.Fatal("nil FS")
	}
	for path, want := range map[string]error{
		"/missing.wasm": runtime.ErrNotFound, "/bin": runtime.ErrInvalid, "/bin/text": runtime.ErrInvalid,
		"/bin/bad.wasm": runtime.ErrInvalid, "/bin/text/x": runtime.ErrInvalid,
	} {
		if _, err := e.Start(ctx, runtime.Spec{FS: f, Path: path}); !errors.Is(err, want) {
			t.Errorf("%s: %v, want %v", path, err, want)
		}
	}
	// The cache is bounded: MaxCompiled 1 keeps one module.
	f.WriteFile("/bin/hello.wasm", testutil.Wasm(t, "hello"), 0o755)
	f.WriteFile("/bin/greet.wasm", testutil.Wasm(t, "greet"), 0o755)
	for _, p := range []string{"/bin/hello.wasm", "/bin/greet.wasm", "/bin/hello.wasm"} {
		proc, err := e.Start(ctx, runtime.Spec{FS: f, Path: p})
		if err != nil {
			t.Fatal(err)
		}
		if res := proc.Wait(); res.ExitCode != 0 {
			t.Fatal(res)
		}
		if e.CacheLen() != 1 {
			t.Fatal("cache over its bound")
		}
	}
	if err := e.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Start(ctx, runtime.Spec{FS: f, Path: "/bin/hello.wasm"}); err == nil {
		t.Fatal("start after close")
	}
}

func TestConcurrentContainers(t *testing.T) {
	img := lower(t, "greet")
	e := New(opts())
	defer e.Close(context.Background())
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			var out syncBuf
			p, err := e.Start(context.Background(), runtime.Spec{FS: img.Clone(rootfs.Quota{}), Path: "/bin/greet.wasm",
				Env: []string{fmt.Sprintf("NAME=c%d", i)}, Stdout: &out})
			if err != nil {
				errs <- err
				return
			}
			p.Wait()
			if out.String() != fmt.Sprintf("Hello, c%d!\n", i) {
				errs <- fmt.Errorf("container %d printed %q", i, out.String())
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestErrnoMapping(t *testing.T) {
	pe := func(err error) error { return &fs.PathError{Op: "x", Path: "p", Err: err} }
	for err, want := range map[error]experimentalsys.Errno{
		nil:                         0,
		pe(fs.ErrNotExist):          experimentalsys.ENOENT,
		pe(fs.ErrExist):             experimentalsys.EEXIST,
		pe(fs.ErrPermission):        experimentalsys.EPERM,
		pe(fs.ErrClosed):            experimentalsys.EBADF,
		pe(rootfs.ErrBadFile):       experimentalsys.EBADF,
		pe(rootfs.ErrNotDir):        experimentalsys.ENOTDIR,
		pe(rootfs.ErrIsDir):         experimentalsys.EISDIR,
		pe(rootfs.ErrNotEmpty):      experimentalsys.ENOTEMPTY,
		pe(rootfs.ErrLoop):          experimentalsys.ELOOP,
		pe(rootfs.ErrReadOnly):      experimentalsys.EROFS,
		pe(rootfs.ErrNameTooLong):   experimentalsys.ENAMETOOLONG,
		pe(fs.ErrInvalid):           experimentalsys.EINVAL,
		pe(rootfs.ErrNoSpace):       experimentalsys.EIO,
		errors.New("anything else"): experimentalsys.EIO,
	} {
		if got := errno(err); got != want {
			t.Errorf("%v: %v, want %v", err, got, want)
		}
	}
	// The adapter's direct surface, beyond what a Go guest exercises.
	f := rootfs.New()
	s := &sysFS{fs: f}
	if s.Mkdir("d", 0o755) != 0 || s.Chmod("d", 0o700) != 0 || s.Unlink("d") != experimentalsys.EISDIR || s.Rmdir("d") != 0 {
		t.Fatal("dir ops")
	}
	h, en := s.OpenFile("f", experimentalsys.O_CREAT|experimentalsys.O_RDWR|experimentalsys.O_EXCL, 0o644)
	if en != 0 {
		t.Fatal(en)
	}
	h.Write([]byte("abc"))
	if n, en := h.Pwrite([]byte("Z"), 0); n != 1 || en != 0 {
		t.Fatal(en)
	}
	buf := make([]byte, 8)
	if n, en := h.Pread(buf, 0); n != 3 || en != 0 || string(buf[:3]) != "Zbc" {
		t.Fatal(n, en)
	}
	if h.Truncate(1) != 0 || h.Utimens(0, experimentalsys.UTIME_OMIT) != 0 || h.Utimens(0, 5) != 0 || h.SetAppend(true) != 0 || !h.IsAppend() {
		t.Fatal("file ops")
	}
	if d, _ := h.Dev(); d != 1 {
		t.Fatal("dev")
	}
	if ino, _ := h.Ino(); ino == 0 {
		t.Fatal("ino")
	}
	if dir, _ := h.IsDir(); dir {
		t.Fatal("isdir")
	}
	if st, en := h.Stat(); en != 0 || st.Size != 1 {
		t.Fatal(st, en)
	}
	if _, en := h.Readdir(1); en != experimentalsys.ENOTDIR {
		t.Fatal(en)
	}
	if h.Close() != 0 || h.Close() != 0 {
		t.Fatal("close")
	}
	if _, en := h.Stat(); en != experimentalsys.EBADF {
		t.Fatal(en)
	}
	if _, en := h.Ino(); en != experimentalsys.EBADF {
		t.Fatal(en)
	}
	if s.Link("f", "g") != 0 || s.Symlink("f", "l") != 0 || s.Rename("g", "h") != 0 {
		t.Fatal("link ops")
	}
	if tgt, en := s.Readlink("l"); tgt != "f" || en != 0 {
		t.Fatal(tgt, en)
	}
	if st, en := s.Lstat("l"); en != 0 || st.Mode&fs.ModeSymlink == 0 {
		t.Fatal(st, en)
	}
	if _, en := s.Lstat("zz"); en != experimentalsys.ENOENT {
		t.Fatal(en)
	}
	if _, en := s.Stat("zz"); en != experimentalsys.ENOENT {
		t.Fatal(en)
	}
	if s.Utimens("f", 0, experimentalsys.UTIME_OMIT) != 0 || s.Utimens("zz", 0, experimentalsys.UTIME_OMIT) != experimentalsys.ENOENT || s.Utimens("f", 0, 7) != 0 {
		t.Fatal("utimens")
	}
	if _, en := s.OpenFile("zz", experimentalsys.O_RDONLY, 0); en != experimentalsys.ENOENT {
		t.Fatal(en)
	}
	if s.Unlink("h") != 0 {
		t.Fatal("unlink")
	}
	d, _ := s.OpenFile(".", experimentalsys.O_RDONLY|experimentalsys.O_DIRECTORY, 0)
	ents, en := d.Readdir(-1)
	if en != 0 || len(ents) != 2 {
		t.Fatal(ents, en)
	}
	if _, en := d.Seek(0, 0); en != 0 {
		t.Fatal(en)
	}
	if _, en := d.Read(buf); en != experimentalsys.EISDIR {
		t.Fatal(en)
	}
	if n, en := d.Pread(buf, 0); n != 0 || en != experimentalsys.EISDIR {
		t.Fatal(en)
	}
	wo, _ := s.OpenFile("f", experimentalsys.O_WRONLY|experimentalsys.O_APPEND|experimentalsys.O_TRUNC|experimentalsys.O_NOFOLLOW, 0)
	if n, en := wo.Read(buf); n != 0 || en != experimentalsys.EBADF {
		t.Fatal(en)
	}
	if pagesFor(0) != 1 || pagesFor(1<<40) != 65536 || pagesFor(1<<20) != 16 {
		t.Fatal("pages")
	}
}
