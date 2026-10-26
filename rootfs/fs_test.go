package rootfs

import (
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func mustNil(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func wantErr(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("got %v, want %v", err, target)
	}
}

func read(t *testing.T, f *FS, p string) string {
	t.Helper()
	b, err := f.ReadFile(p)
	mustNil(t, err)
	return string(b)
}

func TestBasicOperations(t *testing.T) {
	f := New()
	mustNil(t, f.MkdirAll("/a/b", 0o755))
	mustNil(t, f.WriteFile("/a/b/c.txt", []byte("hello"), 0o644))
	if got := read(t, f, "a/b/c.txt"); got != "hello" {
		t.Fatalf("read %q", got)
	}
	wantErr(t, f.Mkdir("/a", 0o755), fs.ErrExist)
	wantErr(t, f.Mkdir("/missing/x", 0o755), fs.ErrNotExist)
	wantErr(t, f.Mkdir("/a/b/c.txt/x", 0o755), ErrNotDir)
	wantErr(t, f.Rmdir("/a"), ErrNotEmpty)
	wantErr(t, f.Remove("/a"), ErrIsDir)
	wantErr(t, f.Rmdir("/a/b/c.txt"), ErrNotDir)
	wantErr(t, f.Remove("/"), fs.ErrInvalid)
	_, err := f.ReadFile("/a")
	wantErr(t, err, ErrIsDir)

	mustNil(t, f.Rename("/a/b/c.txt", "/a/d.txt"))
	if _, err := f.Stat("/a/b/c.txt"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("rename left the source")
	}
	mustNil(t, f.Rename("/a/d.txt", "/a/d.txt"))
	wantErr(t, f.Rename("/a", "/a/b/inside"), fs.ErrInvalid)
	mustNil(t, f.WriteFile("/a/e.txt", []byte("e"), 0o644))
	mustNil(t, f.Rename("/a/e.txt", "/a/d.txt")) // replaces
	if got := read(t, f, "/a/d.txt"); got != "e" {
		t.Fatalf("replaced content %q", got)
	}
	wantErr(t, f.Rename("/a/d.txt", "/a/b"), ErrIsDir)
	wantErr(t, f.Rename("/a/b", "/a/d.txt"), ErrNotDir)
	mustNil(t, f.MkdirAll("/x/y", 0o755))
	wantErr(t, f.Rename("/a/b", "/x"), ErrNotEmpty)
	mustNil(t, f.Rename("/x/y", "/a/b")) // empty dir over empty dir
	wantErr(t, f.Rename("/nope", "/a/z"), fs.ErrNotExist)

	mustNil(t, f.Symlink("d.txt", "/a/link"))
	wantErr(t, f.Symlink("d.txt", "/a/link"), fs.ErrExist)
	wantErr(t, f.Symlink("", "/a/l2"), fs.ErrInvalid)
	tgt, err := f.Readlink("/a/link")
	mustNil(t, err)
	if tgt != "d.txt" || read(t, f, "/a/link") != "e" {
		t.Fatalf("symlink: %q", tgt)
	}
	_, err = f.Readlink("/a/d.txt")
	wantErr(t, err, fs.ErrInvalid)
	li, err := f.Lstat("/a/link")
	mustNil(t, err)
	if li.Mode()&fs.ModeSymlink == 0 || li.Size() != 5 {
		t.Fatalf("lstat %v %d", li.Mode(), li.Size())
	}
	mustNil(t, f.Remove("/a/link"))

	mustNil(t, f.Link("/a/d.txt", "/a/hard"))
	wantErr(t, f.Link("/a", "/a/hd"), fs.ErrPermission)
	wantErr(t, f.Link("/a/d.txt", "/a/hard"), fs.ErrExist)
	mustNil(t, f.WriteFile("/a/hard", []byte("shared"), 0o644))
	if got := read(t, f, "/a/d.txt"); got != "shared" {
		t.Fatalf("hard link not shared: %q", got)
	}
	st, _ := f.Stat("/a/d.txt")
	if st.Nlink != 2 {
		t.Fatalf("nlink %d", st.Nlink)
	}
	mustNil(t, f.Remove("/a/hard"))

	mustNil(t, f.Chmod("/a/d.txt", 0o600))
	st, _ = f.Stat("/a/d.txt")
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("chmod: %v", st.Mode())
	}
	when := time.Unix(1234567890, 0)
	mustNil(t, f.Chtimes("/a/d.txt", when, true))
	st, _ = f.Stat("/a/d.txt")
	if !st.ModTime().Equal(when) {
		t.Fatal("chtimes")
	}
	mustNil(t, f.Chmod("/", 0o700))
	wantErr(t, f.Chmod("/none", 0o700), fs.ErrNotExist)

	ents, err := f.ReadDir("/a")
	mustNil(t, err)
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
		if e.Ino() == 0 {
			t.Fatal("zero inode")
		}
		if _, err := e.Info(); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Join(names, ",") != "b,d.txt" {
		t.Fatalf("readdir %v", names)
	}
	_, err = f.ReadDir("/a/d.txt")
	wantErr(t, err, ErrNotDir)
	_, err = f.ReadDir("/zz")
	wantErr(t, err, fs.ErrNotExist)
	if b, n := f.Usage(); b <= 0 || n <= 0 {
		t.Fatalf("usage %d %d", b, n)
	}
	mustNil(t, f.Rmdir("/a/b"))
}

func TestPathResolutionStaysInside(t *testing.T) {
	f := New()
	mustNil(t, f.MkdirAll("/etc", 0o755))
	mustNil(t, f.WriteFile("/etc/passwd", []byte("guest"), 0o644))
	mustNil(t, f.MkdirAll("/app", 0o755))
	// An absolute target is the container's root, not the host's.
	mustNil(t, f.Symlink("/etc/passwd", "/app/abs"))
	// ".." past the root stays at the root.
	mustNil(t, f.Symlink("../../../../etc/passwd", "/app/up"))
	for _, p := range []string{"/app/abs", "/app/up", "../../etc/passwd", "/../../etc/passwd"} {
		if got := read(t, f, p); got != "guest" {
			t.Fatalf("%s: %q", p, got)
		}
	}
	mustNil(t, f.Symlink("loop2", "/loop1"))
	mustNil(t, f.Symlink("loop1", "/loop2"))
	_, err := f.Stat("/loop1")
	wantErr(t, err, ErrLoop)
	_, err = f.Stat("/etc/passwd/x")
	wantErr(t, err, ErrNotDir)
	_, err = f.Stat("/nope/x")
	wantErr(t, err, fs.ErrNotExist)
	_, err = f.Stat(strings.Repeat("a/", 3000))
	wantErr(t, err, ErrNameTooLong)
	_, err = f.Stat("/" + strings.Repeat("a", 300))
	wantErr(t, err, ErrNameTooLong)
	_, err = f.Stat("a\x00b")
	wantErr(t, err, fs.ErrInvalid)
	if _, err := f.IOFS().Open("../x"); !errors.Is(err, fs.ErrInvalid) {
		t.Fatalf("fs.FS Open accepted an invalid path: %v", err)
	}
	// O_CREATE through a dangling symlink creates its target, as POSIX does.
	mustNil(t, f.Symlink("/app/new", "/dangling"))
	mustNil(t, f.WriteFile("/dangling", []byte("n"), 0o644))
	if got := read(t, f, "/app/new"); got != "n" {
		t.Fatal(got)
	}
}

func TestFSTest(t *testing.T) {
	f := New()
	mustNil(t, f.MkdirAll("/bin", 0o755))
	mustNil(t, f.WriteFile("/bin/app.wasm", []byte("\x00asm"), 0o755))
	mustNil(t, f.MkdirAll("/etc/x", 0o755))
	mustNil(t, f.WriteFile("/etc/motd", []byte("hi\n"), 0o644))
	f.Freeze()
	if err := fstest.TestFS(f.IOFS(), "bin/app.wasm", "etc/motd", "etc/x"); err != nil {
		t.Fatal(err)
	}
}

func TestCopyOnWrite(t *testing.T) {
	lower := New()
	mustNil(t, lower.MkdirAll("/etc", 0o755))
	mustNil(t, lower.WriteFile("/etc/motd", []byte("image"), 0o644))
	mustNil(t, lower.WriteFile("/etc/keep", []byte("k"), 0o644))
	lower.Freeze()
	wantErr(t, lower.WriteFile("/etc/motd", []byte("x"), 0o644), ErrReadOnly)
	wantErr(t, lower.Mkdir("/x", 0o755), ErrReadOnly)
	wantErr(t, lower.Chmod("/", 0o700), ErrReadOnly)

	a := lower.Clone(Quota{})
	b := lower.Clone(Quota{})
	mustNil(t, a.WriteFile("/etc/motd", []byte("container a"), 0o644))
	mustNil(t, a.Remove("/etc/keep"))
	mustNil(t, a.MkdirAll("/tmp/new", 0o755))
	mustNil(t, a.Chmod("/etc", 0o700))
	if read(t, lower, "/etc/motd") != "image" || read(t, b, "/etc/motd") != "image" {
		t.Fatal("a write leaked out of its container")
	}
	if _, err := b.Stat("/etc/keep"); err != nil {
		t.Fatal("a delete leaked out of its container")
	}
	if _, err := lower.Stat("/tmp"); err == nil {
		t.Fatal("a mkdir leaked into the image")
	}
	st, _ := lower.Stat("/etc")
	if st.Mode().Perm() != 0o755 {
		t.Fatal("a chmod leaked into the image")
	}
	if read(t, a, "/etc/motd") != "container a" {
		t.Fatal("the container lost its own write")
	}
	// A clone of an unfrozen tree: both sides copy up from then on.
	c := a.Clone(Quota{})
	mustNil(t, a.WriteFile("/etc/motd", []byte("a again"), 0o644))
	if read(t, c, "/etc/motd") != "container a" {
		t.Fatal("writing the source changed its clone")
	}
	// Opening read-write copies up without changing the lower.
	h, err := b.OpenFile("/etc/motd", O_RDWR, 0)
	mustNil(t, err)
	_, err = h.WriteAt([]byte("I"), 0)
	mustNil(t, err)
	mustNil(t, h.Close())
	if read(t, lower, "/etc/motd") != "image" || read(t, b, "/etc/motd") != "Image" {
		t.Fatal("pwrite copy-up")
	}
}

func TestQuota(t *testing.T) {
	lower := New()
	mustNil(t, lower.WriteFile("/big", make([]byte, 100), 0o644))
	lower.Freeze()
	f := lower.Clone(Quota{MaxBytes: 1000, MaxFiles: 3})
	mustNil(t, f.WriteFile("/a", make([]byte, 500), 0o644))
	wantErr(t, f.WriteFile("/b", make([]byte, 600), 0o644), ErrNoSpace)
	mustNil(t, f.Remove("/b")) // created empty before the write failed
	h, err := f.OpenFile("/c", O_WRONLY|O_CREATE, 0o644)
	mustNil(t, err)
	// A write far past the end must fail before allocating anything.
	_, err = h.WriteAt([]byte("x"), 1<<40)
	wantErr(t, err, ErrNoSpace)
	_, err = h.WriteAt([]byte("x"), -1)
	wantErr(t, err, fs.ErrInvalid)
	wantErr(t, h.Truncate(1<<40), ErrNoSpace)
	mustNil(t, h.Close())
	mustNil(t, f.Mkdir("/d", 0o755))
	wantErr(t, f.Mkdir("/e", 0o755), ErrNoSpace)
	wantErr(t, f.Symlink("x", "/s"), ErrNoSpace)
	mustNil(t, f.Remove("/a")) // frees its bytes
	mustNil(t, f.WriteFile("/c", make([]byte, 800), 0o644))
	// Copy-up of a lower file is charged to the container.
	mustNil(t, f.Remove("/c"))
	h, err = f.OpenFile("/big", O_RDWR, 0)
	mustNil(t, err)
	mustNil(t, h.Close())
	if b, _ := f.Usage(); b != 100 {
		t.Fatalf("copy-up charged %d", b)
	}
}

func TestFileHandles(t *testing.T) {
	f := New()
	mustNil(t, f.WriteFile("/f", []byte("0123456789"), 0o644))
	h, err := f.OpenFile("/f", O_RDWR, 0)
	mustNil(t, err)
	buf := make([]byte, 4)
	n, err := h.Read(buf)
	if n != 4 || err != nil || string(buf) != "0123" {
		t.Fatal(n, err)
	}
	if off, _ := h.Seek(-2, io.SeekEnd); off != 8 {
		t.Fatal(off)
	}
	if off, _ := h.Seek(1, io.SeekCurrent); off != 9 {
		t.Fatal(off)
	}
	_, err = h.Seek(-100, io.SeekCurrent)
	wantErr(t, err, fs.ErrInvalid)
	_, err = h.Seek(0, 99)
	wantErr(t, err, fs.ErrInvalid)
	n, err = h.ReadAt(buf, 8)
	if n != 2 || err != io.EOF {
		t.Fatal(n, err)
	}
	_, err = h.ReadAt(buf, -1)
	wantErr(t, err, fs.ErrInvalid)
	if _, err := h.ReadAt(buf, 100); err != io.EOF {
		t.Fatal(err)
	}
	h.Seek(0, io.SeekStart)
	h.SetAppend(true)
	if !h.IsAppend() {
		t.Fatal("append")
	}
	h.Write([]byte("AB"))
	h.SetAppend(false)
	mustNil(t, h.Truncate(3))
	mustNil(t, h.Truncate(5))
	wantErr(t, h.Truncate(-1), fs.ErrInvalid)
	mustNil(t, h.Chtimes(time.Unix(5, 0)))
	in, err := h.Info()
	mustNil(t, err)
	if in.Size() != 5 || in.Name() != "f" || in.IsDir() || in.Sys() != nil {
		t.Fatal(in)
	}
	mustNil(t, h.Close())
	wantErr(t, h.Close(), fs.ErrClosed)
	_, err = h.Read(buf)
	wantErr(t, err, fs.ErrClosed)
	_, err = h.Stat()
	wantErr(t, err, fs.ErrClosed)
	_, err = h.Seek(0, 0)
	wantErr(t, err, fs.ErrClosed)
	wantErr(t, h.Chtimes(time.Now()), fs.ErrClosed)
	_, err = h.Entries(0)
	wantErr(t, err, fs.ErrClosed)
	if got := read(t, f, "/f"); got != "012\x00\x00" {
		t.Fatalf("%q", got)
	}

	ro, err := f.OpenFile("/f", O_RDONLY, 0)
	mustNil(t, err)
	_, err = ro.Write([]byte("x"))
	wantErr(t, err, ErrBadFile)
	wo, err := f.OpenFile("/f", O_WRONLY, 0)
	mustNil(t, err)
	_, err = wo.Read(buf)
	wantErr(t, err, ErrBadFile)

	_, err = f.OpenFile("/f", O_CREATE|O_EXCL|O_WRONLY, 0o644)
	wantErr(t, err, fs.ErrExist)
	_, err = f.OpenFile("/nope", O_RDONLY, 0)
	wantErr(t, err, fs.ErrNotExist)
	_, err = f.OpenFile("/f", O_DIRECTORY, 0)
	wantErr(t, err, ErrNotDir)
	_, err = f.OpenFile("/nope", O_DIRECTORY, 0)
	wantErr(t, err, fs.ErrNotExist)
	_, err = f.OpenFile("/", O_WRONLY, 0)
	wantErr(t, err, ErrIsDir)
	mustNil(t, f.Symlink("f", "/sl"))
	_, err = f.OpenFile("/sl", O_NOFOLLOW, 0)
	wantErr(t, err, ErrLoop)

	mustNil(t, f.MkdirAll("/d", 0o755))
	for _, n := range []string{"a", "b", "c"} {
		mustNil(t, f.WriteFile("/d/"+n, nil, 0o644))
	}
	d, err := f.OpenFile("/d", O_RDONLY|O_DIRECTORY, 0)
	mustNil(t, err)
	if !d.IsDir() || d.Name() != "d" {
		t.Fatal("dir handle")
	}
	_, err = d.Read(buf)
	wantErr(t, err, ErrIsDir)
	_, err = d.Seek(1, io.SeekStart)
	wantErr(t, err, fs.ErrInvalid)
	e1, _ := d.ReadDir(2)
	e2, _ := d.ReadDir(2)
	if _, err := d.ReadDir(2); err != io.EOF {
		t.Fatal("no EOF at the end of a directory")
	}
	if len(e1) != 2 || len(e2) != 1 {
		t.Fatal(len(e1), len(e2))
	}
	d.Seek(0, io.SeekStart)
	all, _ := d.ReadDir(0)
	if len(all) != 3 {
		t.Fatal(len(all))
	}
	_, err = h.ReadDir(1)
	wantErr(t, err, fs.ErrClosed)
	fh, _ := f.OpenFile("/f", O_RDONLY, 0)
	_, err = fh.Entries(1)
	wantErr(t, err, ErrNotDir)
}
