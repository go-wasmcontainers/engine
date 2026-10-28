package netdriver

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"
)

// Multi is one container on several networks (`docker network connect`):
// an Endpoint over one endpoint per network, in the order they were added.
//
//   - Addr is the first network's address.
//   - A listener or a datagram socket is open on every network, as one
//     bound to 0.0.0.0 is, networks added while it is open included (a
//     network where its port is taken is skipped).
//   - A dial goes out through the network whose prefix holds the address
//     (an endpoint with a Prefix method), else through the first network
//     that reaches it.
//   - Resolve asks every network, the first one's answers first.
type Multi struct {
	// op serialises Listen, ListenPacket, Add and Remove, so a socket
	// opened while a network is added is open on it exactly once.
	op sync.Mutex

	mu       sync.Mutex
	detached bool
	eps      []NamedEndpoint
	ls       map[*multiListener]struct{}
	ps       map[*multiPacket]struct{}
}

// NamedEndpoint is a Multi's endpoint on one network.
type NamedEndpoint struct {
	Network  string
	Endpoint Endpoint
}

// NewMulti returns a Multi on no network.
func NewMulti() *Multi {
	return &Multi{ls: map[*multiListener]struct{}{}, ps: map[*multiPacket]struct{}{}}
}

// Add puts the container on another network.
func (m *Multi) Add(ctx context.Context, network string, ep Endpoint) error {
	if ep == nil {
		return fmt.Errorf("netdriver: no endpoint for %s", network)
	}
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	if m.detached {
		m.mu.Unlock()
		return net.ErrClosed
	}
	for _, e := range m.eps {
		if e.Network == network {
			m.mu.Unlock()
			return fmt.Errorf("netdriver: already on %s", network)
		}
	}
	m.eps = append(m.eps, NamedEndpoint{network, ep})
	ls := make([]*multiListener, 0, len(m.ls))
	for l := range m.ls {
		ls = append(ls, l)
	}
	ps := make([]*multiPacket, 0, len(m.ps))
	for p := range m.ps {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	for _, l := range ls {
		if sub, err := ep.Listen(l.ctx, l.network, l.port); err == nil {
			l.start(ep, sub)
		}
	}
	for _, p := range ps {
		if sub, err := ep.ListenPacket(p.ctx, "udp", p.port); err == nil {
			p.start(ep, sub)
		}
	}
	return nil
}

// Remove takes the container off a network and returns its endpoint,
// which the caller detaches; what was open on it is closed.
func (m *Multi) Remove(network string) (Endpoint, bool) {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	var ep Endpoint
	for i, e := range m.eps {
		if e.Network == network {
			ep = e.Endpoint
			m.eps = append(m.eps[:i:i], m.eps[i+1:]...)
			break
		}
	}
	ls := make([]*multiListener, 0, len(m.ls))
	for l := range m.ls {
		ls = append(ls, l)
	}
	ps := make([]*multiPacket, 0, len(m.ps))
	for p := range m.ps {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	if ep == nil {
		return nil, false
	}
	for _, l := range ls {
		l.drop(ep)
	}
	for _, p := range ps {
		p.drop(ep)
	}
	return ep, true
}

// Endpoints returns the networks and their endpoints, in order.
func (m *Multi) Endpoints() []NamedEndpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]NamedEndpoint(nil), m.eps...)
}

// Addr implements Endpoint: the first network's address.
func (m *Multi) Addr() netip.Addr {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.eps) == 0 {
		return netip.Addr{}
	}
	return m.eps[0].Endpoint.Addr()
}

// prefixed is an endpoint that says its network's range.
type prefixed interface{ Prefix() netip.Prefix }

// route picks the endpoint whose network holds a (the first for the
// loopback and unspecified addresses); nil when no prefix says.
func route(eps []NamedEndpoint, a netip.Addr) Endpoint {
	a = a.Unmap()
	if len(eps) == 0 {
		return nil
	}
	if a.IsLoopback() || a.IsUnspecified() {
		return eps[0].Endpoint
	}
	for _, e := range eps {
		if p, ok := e.Endpoint.(prefixed); ok && p.Prefix().Contains(a) {
			return e.Endpoint
		}
	}
	return nil
}

func unreachable(err error) bool {
	return errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ENETUNREACH)
}

// Dial implements Endpoint.
func (m *Multi) Dial(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error) {
	eps := m.Endpoints()
	if len(eps) == 0 {
		return nil, syscall.ENETDOWN
	}
	if ep := route(eps, addr.Addr()); ep != nil {
		return ep.Dial(ctx, network, addr)
	}
	err := error(syscall.EHOSTUNREACH)
	for _, e := range eps {
		c, derr := e.Endpoint.Dial(ctx, network, addr)
		if derr == nil {
			return c, nil
		}
		if !unreachable(derr) {
			return nil, derr
		}
		err = derr
	}
	return nil, err
}

// Resolve implements Endpoint.
func (m *Multi) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	eps := m.Endpoints()
	if len(eps) == 0 {
		return nil, syscall.ENETDOWN
	}
	if len(eps) == 1 {
		return eps[0].Endpoint.Resolve(ctx, name)
	}
	var out []netip.Addr
	var first error
	seen := map[netip.Addr]bool{}
	for _, e := range eps {
		addrs, err := e.Endpoint.Resolve(ctx, name)
		if err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		for _, a := range addrs {
			if !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	if len(out) == 0 {
		return nil, first
	}
	return out, nil
}

// Detach implements Endpoint: every network's endpoint is detached.
func (m *Multi) Detach() error {
	m.op.Lock()
	defer m.op.Unlock()
	m.mu.Lock()
	m.detached = true
	eps := m.eps
	m.eps = nil
	ls := m.ls
	ps := m.ps
	m.ls = map[*multiListener]struct{}{}
	m.ps = map[*multiPacket]struct{}{}
	m.mu.Unlock()
	for l := range ls {
		l.close()
	}
	for p := range ps {
		p.close()
	}
	var err error
	for _, e := range eps {
		err = errors.Join(err, e.Endpoint.Detach())
	}
	return err
}

func portOf(a net.Addr) uint16 {
	switch a := a.(type) {
	case *net.TCPAddr:
		return uint16(a.Port)
	case *net.UDPAddr:
		return uint16(a.Port)
	}
	return 0
}

// Listen implements Endpoint.
func (m *Multi) Listen(ctx context.Context, network string, port uint16) (net.Listener, error) {
	m.op.Lock()
	defer m.op.Unlock()
	eps := m.Endpoints()
	if len(eps) == 0 {
		return nil, syscall.ENETDOWN
	}
	l := &multiListener{m: m, ctx: ctx, network: network, ch: make(chan net.Conn), done: make(chan struct{}), subs: map[Endpoint]net.Listener{}}
	for i, e := range eps {
		sub, err := e.Endpoint.Listen(ctx, network, port)
		if err != nil {
			l.close()
			return nil, err
		}
		if i == 0 {
			l.addr = sub.Addr()
			if port == 0 {
				port = portOf(sub.Addr())
			}
		}
		l.start(e.Endpoint, sub)
	}
	l.port = port
	m.mu.Lock()
	m.ls[l] = struct{}{}
	m.mu.Unlock()
	return l, nil
}

type multiListener struct {
	m       *Multi
	ctx     context.Context
	network string
	port    uint16
	addr    net.Addr
	ch      chan net.Conn
	done    chan struct{}
	once    sync.Once

	mu   sync.Mutex
	subs map[Endpoint]net.Listener
}

func (l *multiListener) start(ep Endpoint, sub net.Listener) {
	l.mu.Lock()
	select {
	case <-l.done:
		l.mu.Unlock()
		sub.Close()
		return
	default:
	}
	l.subs[ep] = sub
	l.mu.Unlock()
	go func() {
		for {
			c, err := sub.Accept()
			if err != nil {
				return // closed: the listener, or its network
			}
			select {
			case l.ch <- c:
			case <-l.done:
				c.Close()
				return
			}
		}
	}()
}

func (l *multiListener) drop(ep Endpoint) {
	l.mu.Lock()
	sub := l.subs[ep]
	delete(l.subs, ep)
	l.mu.Unlock()
	if sub != nil {
		sub.Close()
	}
}

func (l *multiListener) close() {
	l.once.Do(func() {
		close(l.done)
		l.mu.Lock()
		subs := l.subs
		l.subs = map[Endpoint]net.Listener{}
		l.mu.Unlock()
		for _, s := range subs {
			s.Close()
		}
	})
}

func (l *multiListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.ch:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

func (l *multiListener) Close() error {
	l.close()
	l.m.mu.Lock()
	delete(l.m.ls, l)
	l.m.mu.Unlock()
	return nil
}

func (l *multiListener) Addr() net.Addr { return l.addr }

// ListenPacket implements Endpoint.
func (m *Multi) ListenPacket(ctx context.Context, network string, port uint16) (net.PacketConn, error) {
	if network != "udp" {
		return nil, syscall.EPROTOTYPE
	}
	m.op.Lock()
	defer m.op.Unlock()
	eps := m.Endpoints()
	if len(eps) == 0 {
		return nil, syscall.ENETDOWN
	}
	p := &multiPacket{m: m, ctx: ctx, subs: map[Endpoint]net.PacketConn{}, notify: make(chan struct{})}
	for i, e := range eps {
		sub, err := e.Endpoint.ListenPacket(ctx, network, port)
		if err != nil {
			p.close()
			return nil, err
		}
		if i == 0 {
			p.local = sub.LocalAddr()
			if port == 0 {
				port = portOf(sub.LocalAddr())
			}
		}
		p.start(e.Endpoint, sub)
	}
	p.port = port
	m.mu.Lock()
	m.ps[p] = struct{}{}
	m.mu.Unlock()
	return p, nil
}

// multiReadBuffer is what one network's datagram reader reads into: the
// largest datagram Local and the relay carry, plus one byte so a longer
// one from another driver is seen truncated rather than passed off whole.
// It is host memory per socket and network, so it stays small.
const multiReadBuffer = MaxDatagram + 1

type multiPacket struct {
	m     *Multi
	ctx   context.Context
	port  uint16
	local net.Addr

	mu     sync.Mutex
	subs   map[Endpoint]net.PacketConn
	queue  []datagram
	closed bool
	rdl    time.Time
	notify chan struct{}
}

func (p *multiPacket) changed() {
	close(p.notify)
	p.notify = make(chan struct{})
}

func (p *multiPacket) start(ep Endpoint, sub net.PacketConn) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		sub.Close()
		return
	}
	p.subs[ep] = sub
	p.mu.Unlock()
	go func() {
		buf := make([]byte, multiReadBuffer)
		for {
			k, from, err := sub.ReadFrom(buf)
			if err != nil {
				return // closed: the socket, or its network
			}
			ua, _ := from.(*net.UDPAddr)
			if ua == nil {
				continue
			}
			p.mu.Lock()
			if !p.closed && len(p.queue) < DatagramQueue {
				p.queue = append(p.queue, datagram{ua.AddrPort(), append([]byte(nil), buf[:k]...)})
				p.changed()
			}
			p.mu.Unlock()
		}
	}()
}

func (p *multiPacket) drop(ep Endpoint) {
	p.mu.Lock()
	sub := p.subs[ep]
	delete(p.subs, ep)
	p.mu.Unlock()
	if sub != nil {
		sub.Close()
	}
}

func (p *multiPacket) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.queue = nil
	subs := p.subs
	p.subs = map[Endpoint]net.PacketConn{}
	p.changed()
	p.mu.Unlock()
	for _, s := range subs {
		s.Close()
	}
}

func (p *multiPacket) ReadFrom(b []byte) (int, net.Addr, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		if p.closed {
			return 0, nil, &net.OpError{Op: "read", Net: "udp", Source: p.local, Err: net.ErrClosed}
		}
		if past(p.rdl) {
			return 0, nil, &net.OpError{Op: "read", Net: "udp", Source: p.local, Err: os.ErrDeadlineExceeded}
		}
		if len(p.queue) > 0 {
			d := p.queue[0]
			p.queue = p.queue[1:]
			return copy(b, d.b), net.UDPAddrFromAddrPort(d.from), nil
		}
		ch, dl := p.notify, p.rdl
		p.mu.Unlock()
		err := waitUntil(ch, dl)
		p.mu.Lock()
		if err != nil {
			return 0, nil, &net.OpError{Op: "read", Net: "udp", Source: p.local, Err: err}
		}
	}
}

func (p *multiPacket) WriteTo(b []byte, addr net.Addr) (int, error) {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, &net.OpError{Op: "write", Net: "udp", Err: net.InvalidAddrError("not a UDP address")}
	}
	eps := p.m.Endpoints()
	ep := route(eps, ua.AddrPort().Addr())
	p.mu.Lock()
	sub := p.subs[ep]
	if sub == nil { // no prefix says: the first network still open
		for _, e := range eps {
			if s := p.subs[e.Endpoint]; s != nil {
				sub = s
				break
			}
		}
	}
	p.mu.Unlock()
	if sub == nil {
		return 0, &net.OpError{Op: "write", Net: "udp", Addr: addr, Err: syscall.ENETDOWN}
	}
	return sub.WriteTo(b, addr)
}

func (p *multiPacket) Close() error {
	p.close()
	p.m.mu.Lock()
	delete(p.m.ps, p)
	p.m.mu.Unlock()
	return nil
}

func (p *multiPacket) LocalAddr() net.Addr { return p.local }

func (p *multiPacket) SetDeadline(t time.Time) error { return p.SetReadDeadline(t) }

func (p *multiPacket) SetReadDeadline(t time.Time) error {
	p.mu.Lock()
	p.rdl = t
	p.changed()
	p.mu.Unlock()
	return nil
}

// SetWriteDeadline is accepted: a datagram write does not block here.
func (p *multiPacket) SetWriteDeadline(time.Time) error { return nil }
