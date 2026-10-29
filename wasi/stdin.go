package wasi

import (
	"io"
	"sync"
)

// StdinSize bounds what a process's stdin holds ahead of the guest.
const StdinSize = 64 << 10

// Stdin is a process's standard input as the host serves it: a goroutine
// reads the underlying reader into a bounded buffer, and fd_read and
// poll_oneoff never block on it — when nothing is there yet they ask the
// host to wait (Result.Wait), as for sockets and pipes. In the browser
// that keeps the engine's event loop free: the reader may well be fed by
// messages that loop delivers (an attach stream, a terminal), which a
// blocking Read there would never let in.
//
// A process and the children it spawns share one Stdin (wrap once, with
// NewStdin, and hand the same value on), so no two goroutines compete for
// the underlying reader.
type Stdin struct {
	r io.Reader

	mu      sync.Mutex
	buf     []byte
	eof     bool
	pumping bool
	wake    chan struct{} // closed when data or the end arrives
	space   *sync.Cond    // signalled when the guest takes data
}

// NewStdin wraps r; a *Stdin is returned as is, nil stays nil.
func NewStdin(r io.Reader) *Stdin {
	if r == nil {
		return nil
	}
	if s, ok := r.(*Stdin); ok {
		return s
	}
	s := &Stdin{r: r, wake: make(chan struct{})}
	s.space = sync.NewCond(&s.mu)
	return s
}

// pumpLocked starts the reading goroutine on first use: a process that
// never reads its stdin never takes anything from the reader.
func (s *Stdin) pumpLocked() {
	if s.pumping || s.eof {
		return
	}
	s.pumping = true
	go func() {
		tmp := make([]byte, 4096)
		for {
			s.mu.Lock()
			for len(s.buf) >= StdinSize {
				s.space.Wait()
			}
			room := min(StdinSize-len(s.buf), len(tmp))
			s.mu.Unlock()
			n, err := s.r.Read(tmp[:room])
			s.mu.Lock()
			s.buf = append(s.buf, tmp[:n]...)
			if err != nil { // io.EOF, or a broken source: the end either way
				s.eof = true
			}
			if n > 0 || s.eof {
				close(s.wake)
				s.wake = make(chan struct{})
			}
			done := s.eof
			s.mu.Unlock()
			if done {
				return
			}
		}
	}()
}

// read takes what is buffered into dst. With nothing there, eof says
// whether the input has ended; otherwise wake is closed when it changes.
// It returns 0 bytes only at the end, or with a wake to wait on.
func (s *Stdin) read(dst []byte) (n int, eof bool, wake <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) > 0 {
		n = copy(dst, s.buf)
		s.buf = s.buf[n:]
		if len(s.buf) == 0 {
			s.buf = nil // let a large backing array go
		}
		s.space.Signal()
		return n, false, nil
	}
	if s.eof {
		return 0, true, nil
	}
	s.pumpLocked()
	return 0, false, s.wake
}

// readiness is poll_oneoff's view: ready with data or at the end;
// otherwise wake is closed when that changes.
func (s *Stdin) readiness() (ready, hangup bool, nbytes uint64, wake <-chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buf) > 0 {
		return true, false, uint64(len(s.buf)), nil
	}
	if s.eof {
		return true, true, 0, nil
	}
	s.pumpLocked()
	return false, false, 0, s.wake
}

// stdinRead is fd_read on stdin: what is buffered, the end, or a wait.
func (p *Proc) stdinRead(f *fdEntry, iovs, niov, resultPtr uint32, m Memory) Result {
	if niov > 1024 {
		return fail(EINVAL)
	}
	vec, okv := m.Read(iovs, niov*8)
	if !okv {
		return fail(EFAULT)
	}
	vec = append([]byte(nil), vec...)
	var total uint32
	var wake <-chan struct{}
	if p.stdin != nil {
		for i := uint32(0); i < niov; i++ {
			ptr, ln := le32(vec[i*8:]), le32(vec[i*8+4:])
			if ln == 0 {
				continue
			}
			buf := make([]byte, min(ln, StdinSize))
			n, eof, wk := p.stdin.read(buf)
			if n > 0 && !m.Write(ptr, buf[:n]) {
				return fail(EFAULT)
			}
			total += uint32(n)
			wake = wk
			if eof || n < len(buf) {
				break
			}
		}
	}
	if total == 0 && wake != nil {
		if f.nonblock {
			return fail(EAGAIN)
		}
		return Result{Wait: wake, Resume: func(m Memory) Result {
			if p.fds == nil {
				return fail(EBADF)
			}
			return p.stdinRead(f, iovs, niov, resultPtr, m)
		}}
	}
	if !writeU32(m, resultPtr, total) {
		return fail(EFAULT)
	}
	return ok()
}

// Read blocks until data or the end: for hosts that run the guest on a
// goroutine of its own (wazero's WASI).
func (s *Stdin) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for {
		n, eof, wake := s.read(p)
		if n > 0 {
			return n, nil
		}
		if eof {
			return 0, io.EOF
		}
		<-wake
	}
}
