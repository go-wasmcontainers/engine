package netdriver

import (
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"time"
)

// StreamWindow is how many bytes one direction of a local stream holds
// before its writer blocks (the relay's window is the same).
const StreamWindow = 64 << 10

// pipeBuf is one direction of a stream: a bounded buffer, its writer's
// end of stream, and a channel closed (and replaced) on every change so a
// blocked side can wait with a deadline.
type pipeBuf struct {
	mu       sync.Mutex
	buf      []byte
	eof      bool // the writer closed: EOF once drained
	reset    bool // aborted: ECONNRESET on both sides
	gone     bool // the reader closed: the writer gets EPIPE
	deadline time.Time
	wdead    time.Time
	notify   chan struct{}
}

func newPipeBuf() *pipeBuf { return &pipeBuf{notify: make(chan struct{})} }

type timeT = time.Time

// past reports a deadline that has passed (an operation then fails at
// once, even with data or room available, as net.Conn requires).
func past(deadline time.Time) bool { return !deadline.IsZero() && !time.Now().Before(deadline) }

// waitUntil waits for ch to close or the deadline (zero: none) to pass.
func waitUntil(ch <-chan struct{}, deadline time.Time) error {
	if deadline.IsZero() {
		<-ch
		return nil
	}
	d := time.Until(deadline)
	if d <= 0 {
		return os.ErrDeadlineExceeded
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ch:
		return nil
	case <-t.C:
		return os.ErrDeadlineExceeded
	}
}

// changed wakes every waiter; the caller holds mu.
func (p *pipeBuf) changed() {
	close(p.notify)
	p.notify = make(chan struct{})
}

// wait blocks until the buffer changes or the deadline passes; it is
// called with mu held and returns with it held.
func (p *pipeBuf) wait(deadline time.Time) error {
	ch := p.notify
	p.mu.Unlock()
	defer p.mu.Lock()
	return waitUntil(ch, deadline)
}

// pipeConn is one end of an in-memory stream with TCP's semantics: a
// bounded window per direction, half-close, reset.
type pipeConn struct {
	rd, wr        *pipeBuf
	local, remote net.Addr
	closeOnce     sync.Once
	closed        chan struct{}
}

// streamPair returns the two ends of a stream between a and b.
func streamPair(a, b net.Addr) (*pipeConn, *pipeConn) {
	ab, ba := newPipeBuf(), newPipeBuf()
	return &pipeConn{rd: ba, wr: ab, local: a, remote: b, closed: make(chan struct{})},
		&pipeConn{rd: ab, wr: ba, local: b, remote: a, closed: make(chan struct{})}
}

func (c *pipeConn) isClosed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *pipeConn) opErr(op string, err error) error {
	return &net.OpError{Op: op, Net: c.local.Network(), Source: c.local, Addr: c.remote, Err: err}
}

func (c *pipeConn) Read(b []byte) (int, error) {
	p := c.rd
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		switch {
		case c.isClosed():
			return 0, c.opErr("read", net.ErrClosed)
		case past(p.deadline):
			return 0, c.opErr("read", os.ErrDeadlineExceeded)
		case p.reset:
			return 0, c.opErr("read", syscall.ECONNRESET)
		case len(p.buf) > 0:
			if len(b) == 0 {
				return 0, nil
			}
			n := copy(b, p.buf)
			p.buf = p.buf[n:]
			if len(p.buf) == 0 {
				p.buf = nil
			}
			p.changed()
			return n, nil
		case p.eof:
			return 0, io.EOF
		}
		if err := p.wait(p.deadline); err != nil {
			return 0, c.opErr("read", err)
		}
	}
}

func (c *pipeConn) Write(b []byte) (int, error) {
	p := c.wr
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for {
		switch {
		case c.isClosed():
			return n, c.opErr("write", net.ErrClosed)
		case past(p.wdead):
			return n, c.opErr("write", os.ErrDeadlineExceeded)
		case p.reset:
			return n, c.opErr("write", syscall.ECONNRESET)
		case p.eof:
			return n, c.opErr("write", syscall.EPIPE) // after CloseWrite
		case p.gone:
			return n, c.opErr("write", syscall.EPIPE)
		case n == len(b):
			return n, nil
		}
		if room := StreamWindow - len(p.buf); room > 0 {
			k := min(room, len(b)-n)
			p.buf = append(p.buf, b[n:n+k]...)
			n += k
			p.changed()
			continue
		}
		if err := p.wait(p.wdead); err != nil {
			return n, c.opErr("write", err)
		}
	}
}

// CloseWrite half-closes: the peer reads EOF once it has drained.
func (c *pipeConn) CloseWrite() error {
	p := c.wr
	p.mu.Lock()
	defer p.mu.Unlock()
	p.eof = true
	p.changed()
	return nil
}

// CloseRead stops reading: the peer's writes fail with EPIPE.
func (c *pipeConn) CloseRead() error {
	p := c.rd
	p.mu.Lock()
	defer p.mu.Unlock()
	p.gone = true
	p.buf = nil
	p.changed()
	return nil
}

func (c *pipeConn) Close() error {
	c.closeOnce.Do(func() {
		close(c.closed)
		c.CloseWrite()
		c.CloseRead()
	})
	return nil
}

// Reset aborts the stream: both ends get ECONNRESET.
func (c *pipeConn) Reset() {
	for _, p := range []*pipeBuf{c.rd, c.wr} {
		p.mu.Lock()
		p.reset = true
		p.buf = nil
		p.changed()
		p.mu.Unlock()
	}
}

func (c *pipeConn) LocalAddr() net.Addr  { return c.local }
func (c *pipeConn) RemoteAddr() net.Addr { return c.remote }

func (c *pipeConn) SetDeadline(t time.Time) error {
	c.SetReadDeadline(t)
	return c.SetWriteDeadline(t)
}

func (c *pipeConn) SetReadDeadline(t time.Time) error {
	p := c.rd
	p.mu.Lock()
	p.deadline = t
	p.changed()
	p.mu.Unlock()
	return nil
}

func (c *pipeConn) SetWriteDeadline(t time.Time) error {
	p := c.wr
	p.mu.Lock()
	p.wdead = t
	p.changed()
	p.mu.Unlock()
	return nil
}
