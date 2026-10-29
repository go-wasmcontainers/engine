package portmap

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"
)

// Virtual is a Host with no sockets: what a page is, where an engine runs
// in a browser. A published port is a listener known by its host port, and
// a connection to it is made in memory (Dial) -- by the page's HTTP route
// to a container, say. The host address of a binding means nothing here:
// one port is one listener, whatever address it was asked on.
//
// Only TCP: a page has no use for a UDP port, and ListenPacket refuses.
type Virtual struct {
	mu   sync.Mutex
	ls   map[uint16]*vListener
	next uint16
}

// VirtualBacklog is how many connections wait for Accept before a Dial is
// refused, as a listening socket's backlog refuses.
const VirtualBacklog = 64

// NewVirtual returns a Virtual with nothing listening.
func NewVirtual() *Virtual { return &Virtual{ls: map[uint16]*vListener{}, next: ephemeralFirst} }

const ephemeralFirst = 49152

// Listen implements Host.
func (v *Virtual) Listen(ctx context.Context, network string, addr netip.AddrPort) (net.Listener, error) {
	if network != "tcp" {
		return nil, syscall.EPROTONOSUPPORT
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	port := addr.Port()
	if port == 0 {
		p, ok := v.freeLocked()
		if !ok {
			return nil, syscall.EADDRINUSE
		}
		port = p
	}
	if v.ls[port] != nil {
		return nil, syscall.EADDRINUSE
	}
	l := &vListener{v: v, addr: &net.TCPAddr{IP: addr.Addr().AsSlice(), Port: int(port)}, port: port,
		queue: make(chan net.Conn, VirtualBacklog), done: make(chan struct{})}
	v.ls[port] = l
	return l, nil
}

// freeLocked picks an ephemeral port no listener holds.
func (v *Virtual) freeLocked() (uint16, bool) {
	for i := 0; i < 65536-ephemeralFirst; i++ {
		p := v.next
		if v.next == 65535 {
			v.next = ephemeralFirst
		} else {
			v.next++
		}
		if v.ls[p] == nil {
			return p, true
		}
	}
	return 0, false
}

// ListenPacket implements Host: refused, a page has no UDP.
func (v *Virtual) ListenPacket(context.Context, string, netip.AddrPort) (net.PacketConn, error) {
	return nil, syscall.EPROTONOSUPPORT
}

// Ports are the host ports listening now.
func (v *Virtual) Ports() []uint16 {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := make([]uint16, 0, len(v.ls))
	for p := range v.ls {
		out = append(out, p)
	}
	return out
}

// Dial connects to the listener on port: ECONNREFUSED when nothing listens
// there or its backlog is full.
func (v *Virtual) Dial(ctx context.Context, port uint16) (net.Conn, error) {
	v.mu.Lock()
	l := v.ls[port]
	v.mu.Unlock()
	if l == nil {
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	client, server := net.Pipe()
	// Under the listener's lock, so a Dial and a Close cannot cross: once
	// Close has drained the queue, nothing is enqueued after it -- a
	// connection is either Accepted or closed, never left in a dead queue
	// for its client to wait on until a timeout.
	l.mu.Lock()
	queued := false
	if !l.closed {
		select {
		case l.queue <- server:
			queued = true
		default: // the backlog is full
		}
	}
	l.mu.Unlock()
	if queued {
		return client, nil
	}
	client.Close()
	server.Close()
	return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
}

// Transport is an http.Transport whose connections go to the port of the
// request's URL ("http://localhost:8080/x" reaches host port 8080): a
// fresh connection per request, so a slow or broken container keeps
// nothing open, and bounded response headers.
func (v *Virtual) Transport() *http.Transport {
	return &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, ps, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			p, err := strconv.ParseUint(ps, 10, 16)
			if err != nil || p == 0 {
				return nil, &net.AddrError{Err: "invalid port", Addr: addr}
			}
			return v.Dial(ctx, uint16(p))
		},
		DisableKeepAlives:      true,
		MaxResponseHeaderBytes: 1 << 20,
		ResponseHeaderTimeout:  60 * time.Second,
		Proxy:                  nil,
	}
}

type vListener struct {
	v     *Virtual
	addr  *net.TCPAddr
	port  uint16
	queue chan net.Conn
	done  chan struct{}
	once  sync.Once

	mu     sync.Mutex // Dial's enqueue and Close's drain
	closed bool
}

func (l *vListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.queue:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *vListener) Close() error {
	l.once.Do(func() {
		l.v.mu.Lock()
		if l.v.ls[l.port] == l {
			delete(l.v.ls, l.port)
		}
		l.v.mu.Unlock()
		l.mu.Lock()
		l.closed = true
		close(l.done)
		for drained := false; !drained; {
			select {
			case c := <-l.queue:
				c.Close()
			default:
				drained = true
			}
		}
		l.mu.Unlock()
	})
	return nil
}

func (l *vListener) Addr() net.Addr { return l.addr }
