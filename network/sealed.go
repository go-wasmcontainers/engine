package network

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
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
// learnt after the network was added (a relay's Welcome) is caught.
type sealed struct {
	netdriver.Endpoint
	name  string
	drv   netdriver.NetworkDriver
	socks *socks
}

// socks is every socket an endpoint opened and has not closed: they are all
// closed the moment the network is found to have egress.
type socks struct {
	mu  sync.Mutex
	set map[io.Closer]struct{}
}

func (k *socks) add(c io.Closer) {
	k.mu.Lock()
	if k.set == nil {
		k.set = map[io.Closer]struct{}{}
	}
	k.set[c] = struct{}{}
	k.mu.Unlock()
}

func (k *socks) del(c io.Closer) {
	k.mu.Lock()
	delete(k.set, c)
	k.mu.Unlock()
}

func (k *socks) closeAll() {
	k.mu.Lock()
	all := k.set
	k.set = nil
	k.mu.Unlock()
	for c := range all {
		c.Close()
	}
}

// guard is the check every socket repeats before it sends or accepts.
// Egress learnt since the socket was opened closes all of the endpoint's
// sockets. The check precedes the call: a write already past it when the
// flip lands is not recalled.
func (s sealed) guard() error {
	if err := s.check(); err != nil {
		s.socks.closeAll()
		return err
	}
	return nil
}

type sealedConn struct {
	net.Conn
	s sealed
}

func (c sealedConn) Write(b []byte) (int, error) {
	if err := c.s.guard(); err != nil {
		return 0, err
	}
	return c.Conn.Write(b)
}

func (c sealedConn) Close() error {
	c.s.socks.del(c)
	return c.Conn.Close()
}

type sealedPacket struct {
	net.PacketConn
	s sealed
}

func (c sealedPacket) WriteTo(b []byte, a net.Addr) (int, error) {
	if err := c.s.guard(); err != nil {
		return 0, err
	}
	return c.PacketConn.WriteTo(b, a)
}

func (c sealedPacket) Close() error {
	c.s.socks.del(c)
	return c.PacketConn.Close()
}

type sealedListener struct {
	net.Listener
	s sealed
}

func (l sealedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	// Checked after the accept: a peer that connected once egress was learnt
	// is refused, and so is every other socket of the endpoint.
	if err := l.s.guard(); err != nil {
		c.Close()
		return nil, err
	}
	sc := sealedConn{c, l.s}
	l.s.socks.add(sc)
	return sc, nil
}

func (l sealedListener) Close() error {
	l.s.socks.del(l)
	return l.Listener.Close()
}

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
	if p, ok := ep.(interface{ Prefix() netip.Prefix }); ok {
		return sealedPrefixed{s, p}, nil
	}
	return s, nil
}

func (s sealed) check() error {
	if !netdriver.Confined(s.drv) || netdriver.IsRemote(s.Endpoint) {
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
	sl := sealedListener{l, s}
	s.socks.add(sl)
	return sl, nil
}

func (s sealed) Dial(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	c, err := s.Endpoint.Dial(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	sc := sealedConn{c, s}
	s.socks.add(sc)
	return sc, nil
}

func (s sealed) ListenPacket(ctx context.Context, network string, port uint16) (net.PacketConn, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	p, err := s.Endpoint.ListenPacket(ctx, network, port)
	if err != nil {
		return nil, err
	}
	sp := sealedPacket{p, s}
	s.socks.add(sp)
	return sp, nil
}

func (s sealed) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	if err := s.guard(); err != nil {
		return nil, err
	}
	return s.Endpoint.Resolve(ctx, name)
}
