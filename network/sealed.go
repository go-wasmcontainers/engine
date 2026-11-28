package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// ErrEgress is an internal network asked to carry traffic out of the engine:
// a driver that does not declare it cannot, or an endpoint that reports it
// is remote. Calls it refuses unwrap to syscall.ENETUNREACH as well.
var ErrEgress = errors.New("network: internal network has no route out")

// admit is the one test of an internal network: the driver must POSITIVELY
// declare that it cannot carry traffic out (netdriver.Confined). Unknown is
// refused, so a wrapper cannot launder a driver's egress.
func admit(name string, d netdriver.NetworkDriver) error {
	if !netdriver.Confined(d) {
		return fmt.Errorf("%w: network %s is internal and its driver does not declare that it has no egress: %w", ErrInvalid, name, ErrEgress)
	}
	return nil
}

// sealed is an endpoint on an internal network. Its driver is asked again at
// every call, and an endpoint that reports Remote is refused, so egress
// learnt after the network was added (a relay's Welcome) is caught. A driver
// that implements netdriver.EgressNotifier also has every socket closed the
// moment it says so, without waiting for the next call.
type sealed struct {
	netdriver.Endpoint
	name  string
	drv   netdriver.NetworkDriver
	socks *socks
}

// sock is one open socket of an endpoint. It is the key of the set (a
// pointer, so a driver that cannot be hashed cannot panic the map).
type sock struct {
	k *socks
	c io.Closer
}

func (t *sock) Close() error {
	t.k.del(t)
	return t.c.Close()
}

// socks is every socket an endpoint opened and has not closed: they are all
// closed the moment the network is found to have egress.
type socks struct {
	mu   sync.Mutex
	set  map[*sock]struct{}
	stop func() // unregisters the egress notification
	// tripped: the driver notified that it has egress. Permanent.
	tripped atomic.Bool
}

// add records c unless the network has egress by now. The test is made under
// the lock closeAll takes, so a socket opened while the network flips is
// either in the set when closeAll runs, or refused here: none survives.
func (k *socks) add(c io.Closer, check func() error) (*sock, error) {
	k.mu.Lock()
	if k.set == nil {
		k.set = map[*sock]struct{}{}
	}
	t := &sock{k, c}
	k.set[t] = struct{}{}
	k.mu.Unlock()
	// The driver is asked with no lock of ours held (it may hold its own
	// while it notifies), AFTER the socket is in the set: egress that lands
	// before this test finds it, one that lands after finds it in the set.
	if err := check(); err != nil {
		k.closeAll() // egress is learnt: the other sockets go too
		return nil, err
	}
	return t, nil
}

// notified is the driver's notice that it carries traffic out. The notice IS
// the verdict: it is not checked against EgressEnabled, and the driver is not
// called (it may call us under its own lock). The sockets are closed on
// another goroutine.
func (k *socks) notified() {
	k.tripped.Store(true)
	go k.closeAll()
}

func (k *socks) del(t *sock) {
	k.mu.Lock()
	delete(k.set, t)
	k.mu.Unlock()
}

func (k *socks) closeAll() {
	k.mu.Lock()
	all := k.set
	k.set = nil
	k.mu.Unlock()
	for t := range all {
		t.c.Close()
	}
}

func (k *socks) unnotify() {
	k.mu.Lock()
	stop := k.stop
	k.stop = nil
	k.mu.Unlock()
	if stop != nil {
		stop()
	}
}

// guard is the check every socket repeats before it sends, receives or
// accepts. Egress learnt since the socket was opened closes all of the
// endpoint's sockets. The check precedes the call: a write already past it
// when the flip lands is not recalled.
func (s sealed) guard() error {
	if err := s.check(); err != nil {
		s.socks.closeAll()
		return err
	}
	return nil
}

// Detach stops listening for the driver's egress and lets the endpoint go.
func (s sealed) Detach() error {
	s.socks.unnotify()
	return s.Endpoint.Detach()
}

// sealedConn guards a stream or connected datagram socket. It is wrapped
// again (wrapConn) so that the optional interfaces the inner conn has
// (CloseWrite, CloseRead, net.PacketConn) survive: shutdown(SHUT_WR) and a
// UDP Dial's use as a PacketConn depend on them. Not preserved: SyscallConn,
// io.ReaderFrom / io.WriterTo and the *net.UDPConn-specific methods
// (ReadFromUDP, ...); nothing in this repository uses them.
type sealedConn struct {
	net.Conn
	s sealed
	t *sock
}

func (c sealedConn) Read(b []byte) (int, error) {
	if err := c.s.guard(); err != nil {
		return 0, err
	}
	return c.Conn.Read(b)
}

func (c sealedConn) Write(b []byte) (int, error) {
	if err := c.s.guard(); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

func (c sealedConn) Close() error { return c.t.Close() }

type closeWriter struct {
	x interface{ CloseWrite() error }
}

func (m closeWriter) CloseWrite() error { return m.x.CloseWrite() }

type closeReader struct {
	x interface{ CloseRead() error }
}

func (m closeReader) CloseRead() error { return m.x.CloseRead() }

// packetConn keeps ReadFrom and WriteTo of a conn that is also a PacketConn.
type packetConn struct {
	x net.PacketConn
	s sealed
}

func (m packetConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if err := m.s.guard(); err != nil {
		return 0, nil, err
	}
	return m.x.ReadFrom(b)
}

func (m packetConn) WriteTo(b []byte, a net.Addr) (int, error) {
	if err := m.s.guard(); err != nil {
		return 0, err
	}
	return m.x.WriteTo(b, a)
}

type (
	connW struct {
		sealedConn
		closeWriter
	}
	connR struct {
		sealedConn
		closeReader
	}
	connWR struct {
		sealedConn
		closeWriter
		closeReader
	}
	connP struct {
		sealedConn
		packetConn
	}
	connPW struct {
		sealedConn
		packetConn
		closeWriter
	}
	connPR struct {
		sealedConn
		packetConn
		closeReader
	}
	connPWR struct {
		sealedConn
		packetConn
		closeWriter
		closeReader
	}
)

// wrapConn returns c guarded, with the optional interfaces of c kept.
func wrapConn(c net.Conn, s sealed) (net.Conn, error) {
	t, err := s.socks.add(c, s.check)
	if err != nil {
		return nil, err
	}
	b := sealedConn{c, s, t}
	cw, w := c.(interface{ CloseWrite() error })
	cr, r := c.(interface{ CloseRead() error })
	pc, p := c.(net.PacketConn)
	W, R, P := closeWriter{cw}, closeReader{cr}, packetConn{pc, s}
	switch {
	case p && w && r:
		return connPWR{b, P, W, R}, nil
	case p && w:
		return connPW{b, P, W}, nil
	case p && r:
		return connPR{b, P, R}, nil
	case p:
		return connP{b, P}, nil
	case w && r:
		return connWR{b, W, R}, nil
	case w:
		return connW{b, W}, nil
	case r:
		return connR{b, R}, nil
	}
	return b, nil
}

type sealedPacket struct {
	net.PacketConn
	s sealed
	t *sock
}

func (c sealedPacket) ReadFrom(b []byte) (int, net.Addr, error) {
	if err := c.s.guard(); err != nil {
		return 0, nil, err
	}
	return c.PacketConn.ReadFrom(b)
}

func (c sealedPacket) WriteTo(b []byte, a net.Addr) (int, error) {
	if err := c.s.guard(); err != nil {
		return 0, err
	}
	return c.PacketConn.WriteTo(b, a)
}

func (c sealedPacket) Close() error { return c.t.Close() }

type sealedListener struct {
	net.Listener
	s sealed
	t *sock
}

func (l sealedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	// Checked after the accept, when the conn is registered: a peer that
	// connected once egress was learnt is refused, and so is every other
	// socket of the endpoint.
	return wrapConn(c, l.s)
}

func (l sealedListener) Close() error { return l.t.Close() }

// sealedPrefixed keeps the Prefix of an endpoint that has one, which
// netdriver.Multi routes by.
type sealedPrefixed struct {
	sealed
	prefix interface{ Prefix() netip.Prefix }
}

func (s sealedPrefixed) Prefix() netip.Prefix { return s.prefix.Prefix() }

// seal returns ep refused unless its network is confined now.
func seal(name string, drv netdriver.NetworkDriver, ep netdriver.Endpoint) (netdriver.Endpoint, error) {
	s := sealed{ep, name, drv, &socks{}}
	if err := s.check(); err != nil {
		ep.Detach()
		return nil, err
	}
	if n, ok := drv.(netdriver.EgressNotifier); ok {
		stop := n.NotifyEgress(s.socks.notified)
		s.socks.mu.Lock()
		s.socks.stop = stop
		s.socks.mu.Unlock()
	}
	if p, ok := ep.(interface{ Prefix() netip.Prefix }); ok {
		return sealedPrefixed{s, p}, nil
	}
	return s, nil
}

func (s sealed) check() error {
	if s.socks.tripped.Load() || !netdriver.Confined(s.drv) || netdriver.IsRemote(s.Endpoint) {
		return fmt.Errorf("%w: network %s: %w", ErrEgress, s.name, syscall.ENETUNREACH)
	}
	return nil
}

func (s sealed) Listen(ctx context.Context, network string, port uint16) (net.Listener, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	l, err := s.Endpoint.Listen(ctx, network, port)
	if err != nil {
		return nil, err
	}
	t, err := s.socks.add(l, s.check)
	if err != nil {
		return nil, err
	}
	return sealedListener{l, s, t}, nil
}

func (s sealed) Dial(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	c, err := s.Endpoint.Dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	return wrapConn(c, s)
}

func (s sealed) ListenPacket(ctx context.Context, network string, port uint16) (net.PacketConn, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	p, err := s.Endpoint.ListenPacket(ctx, network, port)
	if err != nil {
		return nil, err
	}
	t, err := s.socks.add(p, s.check)
	if err != nil {
		return nil, err
	}
	return sealedPacket{p, s, t}, nil
}

func (s sealed) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return s.Endpoint.Resolve(ctx, name)
}
