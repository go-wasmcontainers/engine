package wasi

import (
	"encoding/binary"
	"io"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
)

// noBlock runs one host call and fails if it does not return at once: in
// the browser a host call runs on the engine's event loop, and a blocked
// one freezes the engine (the stdin that would unblock it arrives by a
// message that loop never gets to deliver).
func noBlock(t *testing.T, what string, f func() Result) Result {
	t.Helper()
	got := make(chan Result, 1)
	go func() { got <- f() }()
	select {
	case r := <-got:
		return r
	case <-time.After(5 * time.Second):
		t.Fatalf("%s blocked the host call", what)
		return Result{}
	}
}

// TestStdinNeverBlocks: fd_read and poll_oneoff on a stdin with nothing
// in it ask the host to wait instead of blocking; data written later
// wakes them; the end reads as 0 bytes, and only the end does.
func TestStdinNeverBlocks(t *testing.T) {
	pr, pw := io.Pipe()
	p := New(Config{FS: rootfs.New(), Stdin: pr})
	m := make(mem, 8192)
	iov(m, 100, 1000, 64)

	r := noBlock(t, "fd_read on an empty stdin", func() Result { return call(t, p, m, "fd_read", 0, 100, 1, 200) })
	if r.Wait == nil || r.Errno != 0 {
		t.Fatalf("fd_read on an empty stdin: %+v", r)
	}
	sub(m, 2000, 7, eventFdRead, 0, 0)
	pr2 := noBlock(t, "poll_oneoff on an empty stdin", func() Result { return call(t, p, m, "poll_oneoff", 2000, 3000, 1, 300) })
	if pr2.Wait == nil {
		t.Fatalf("poll on an empty stdin: %+v", pr2)
	}

	go pw.Write([]byte("echo hi\n"))
	select {
	case <-r.Wait:
	case <-time.After(5 * time.Second):
		t.Fatal("data did not wake the read")
	}
	r = noBlock(t, "the resumed read", func() Result { return r.Resume(m) })
	if r.Wait != nil || r.Errno != 0 || binary.LittleEndian.Uint32(m[200:]) != 8 || string(m[1000:1008]) != "echo hi\n" {
		t.Fatalf("resumed read: %+v n=%d %q", r, binary.LittleEndian.Uint32(m[200:]), m[1000:1008])
	}
	// The poll's wake fired too; resumed, nothing is buffered now, so it
	// waits again rather than claim readiness.
	if y := pr2.Resume(m); y.Wait == nil {
		t.Fatalf("poll after the data was taken: %+v", y)
	}

	// Data waiting: poll reports it with its size.
	go pw.Write([]byte("abc"))
	r = call(t, p, m, "fd_read", 0, 100, 1, 200)
	for r.Wait != nil {
		<-r.Wait
		r = r.Resume(m)
	}
	if binary.LittleEndian.Uint32(m[200:]) != 3 {
		t.Fatalf("second read: %d bytes", binary.LittleEndian.Uint32(m[200:]))
	}

	pw.Close()
	r = call(t, p, m, "fd_read", 0, 100, 1, 200)
	for r.Wait != nil {
		<-r.Wait
		r = r.Resume(m)
	}
	if r.Errno != 0 || binary.LittleEndian.Uint32(m[200:]) != 0 {
		t.Fatalf("end of stdin: %+v n=%d", r, binary.LittleEndian.Uint32(m[200:]))
	}
	sub(m, 2000, 7, eventFdRead, 0, 0)
	if y := call(t, p, m, "poll_oneoff", 2000, 3000, 1, 300); y.Wait != nil || binary.LittleEndian.Uint16(m[3000+24:]) != eventHangup {
		t.Fatalf("poll at the end: %+v", y)
	}
}

// TestStdinBounded: a source faster than the guest fills the buffer up to
// StdinSize and no further; the guest then reads all of it in order.
func TestStdinBounded(t *testing.T) {
	pr, pw := io.Pipe()
	s := NewStdin(pr)
	if NewStdin(s) != s || NewStdin(nil) != nil {
		t.Fatal("NewStdin wraps twice")
	}
	go func() {
		b := make([]byte, 1024)
		for i := 0; i < 4*StdinSize/len(b); i++ {
			for j := range b {
				b[j] = byte(i)
			}
			pw.Write(b)
		}
		pw.Close()
	}()
	s.read(nil) // start the pump
	time.Sleep(100 * time.Millisecond)
	s.mu.Lock()
	held := len(s.buf)
	s.mu.Unlock()
	if held > StdinSize {
		t.Fatalf("holds %d bytes, bound %d", held, StdinSize)
	}
	all, err := io.ReadAll(s)
	if err != nil || len(all) != 4*StdinSize {
		t.Fatalf("read %d bytes: %v", len(all), err)
	}
	for i, c := range all {
		if c != byte(i/1024) {
			t.Fatalf("byte %d is %d", i, c)
		}
	}
}
