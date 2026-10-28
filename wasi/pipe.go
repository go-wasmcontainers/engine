package wasi

import "sync"

// Pipes: bounded in-engine pipes between the processes of a container
// (fd_pipe in the WASIX module, shell pipelines). A pipe is shared by
// every descriptor that refers to one of its ends, in any process; it
// counts them, so a reader sees end of file once every write end is
// closed everywhere, and a writer gets EPIPE once no read end remains.
// A blocked read or write returns a Result.Wait on the pipe's change
// channel, like a socket, so it never blocks the engine.

// PipeSize is a pipe's capacity in bytes (Linux's default).
const PipeSize = 64 << 10

type pipe struct {
	mu      sync.Mutex
	buf     []byte // queued bytes, at most PipeSize
	readers int    // descriptors on the read end, in every process
	writers int
	change  chan struct{} // closed and replaced on every change
}

func newPipe() *pipe { return &pipe{change: make(chan struct{})} }

// changedLocked wakes everything waiting on the pipe.
func (q *pipe) changedLocked() {
	close(q.change)
	q.change = make(chan struct{})
}

// ref adds a descriptor on one end.
func (q *pipe) ref(write bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if write {
		q.writers++
	} else {
		q.readers++
	}
}

// unref drops a descriptor on one end.
func (q *pipe) unref(write bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if write {
		q.writers--
	} else {
		q.readers--
	}
	q.changedLocked()
}

// readiness reports whether a read (or write) would not block, whether
// the other end is gone, and the bytes available to read (or free).
func (q *pipe) readiness(write bool) (ready, hangup bool, n uint64, wake <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if write {
		free := PipeSize - len(q.buf)
		return free > 0 || q.readers == 0, q.readers == 0, uint64(max(free, 0)), q.change
	}
	return len(q.buf) > 0 || q.writers == 0, q.writers == 0, uint64(len(q.buf)), q.change
}

// read moves up to len(b) bytes out. It returns n=0 and eof at end of
// file, or wouldBlock with the channel to wait on.
func (q *pipe) read(b []byte) (n int, eof bool, wake <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.buf) == 0 {
		if q.writers == 0 {
			return 0, true, nil
		}
		return 0, false, q.change
	}
	n = copy(b, q.buf)
	q.buf = q.buf[n:]
	if len(q.buf) == 0 {
		q.buf = nil // let the backing array go
	}
	q.changedLocked()
	return n, false, nil
}

// write queues up to the free space. broken reports that no reader
// remains (EPIPE); wake is set when the pipe is full.
func (q *pipe) write(b []byte) (n int, broken bool, wake <-chan struct{}) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.readers == 0 {
		return 0, true, nil
	}
	free := PipeSize - len(q.buf)
	if free <= 0 {
		return 0, false, q.change
	}
	n = min(free, len(b))
	q.buf = append(q.buf, b[:n]...)
	q.changedLocked()
	return n, false, nil
}

// anyOf returns a channel closed when any of chs is, and stop, which ends
// its goroutines. A waiter calls stop once it is done waiting (woken, or
// its clock ran out); otherwise a poll that times out on idle pipes would
// leave its goroutines behind until those pipes change.
func anyOf(chs []<-chan struct{}) (<-chan struct{}, func()) {
	out := make(chan struct{})
	quit := make(chan struct{})
	var once, stopOnce sync.Once
	for _, c := range chs {
		go func(c <-chan struct{}) {
			select {
			case <-c:
				once.Do(func() { close(out) })
			case <-out:
			case <-quit:
			}
		}(c)
	}
	return out, func() { stopOnce.Do(func() { close(quit) }) }
}

// Pipe creates a pipe in this process: the read end at r, the write end
// at w (fd_pipe). It fails with EMFILE when the descriptor table is full.
func (p *Proc) Pipe() (r, w uint32, errno uint32) {
	if len(p.fds)+2 > MaxFDs {
		return 0, 0, EMFILE
	}
	q := newPipe()
	q.readers, q.writers = 1, 1
	r = p.alloc(&fdEntry{kind: kindPipe, pipe: q, read: true})
	w = p.alloc(&fdEntry{kind: kindPipe, pipe: q, pipeW: true, write: true})
	return r, w, 0
}

// pipeRW reads or writes a pipe end, gathering or scattering iovecs. A
// call that would block waits on the pipe (or fails EAGAIN on a
// non-blocking descriptor); a partial transfer returns at once.
func (p *Proc) pipeRW(f *fdEntry, iovs, niov uint32, write bool, resultPtr uint32, m Memory) Result {
	if write != f.pipeW {
		return fail(EBADF)
	}
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
	for i := uint32(0); i < niov; i++ {
		ptr, ln := le32(vec[i*8:]), le32(vec[i*8+4:])
		if ln == 0 {
			continue
		}
		ln = min(ln, PipeSize)
		if write {
			b, okb := m.Read(ptr, ln)
			if !okb {
				return fail(EFAULT)
			}
			n, broken, wk := f.pipe.write(b)
			if broken {
				if total > 0 {
					break
				}
				return fail(EPIPE)
			}
			total += uint32(n)
			wake = wk
			if n < int(ln) {
				break
			}
		} else {
			buf := make([]byte, ln)
			n, eof, wk := f.pipe.read(buf)
			if n > 0 && !m.Write(ptr, buf[:n]) {
				return fail(EFAULT)
			}
			total += uint32(n)
			wake = wk
			if eof || n < int(ln) {
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
			return p.pipeRW(f, iovs, niov, write, resultPtr, m)
		}}
	}
	if !writeU32(m, resultPtr, total) {
		return fail(EFAULT)
	}
	return ok()
}
