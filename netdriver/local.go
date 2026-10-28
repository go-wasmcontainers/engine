package netdriver

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"slices"
	"strings"
	"sync"
	"syscall"
)

// Local is a network inside the engine: each attached container gets an
// address in Prefix, containers reach each other's listeners by address or
// by name (Resolve), and 127.0.0.1 is the container itself. Streams are
// in-memory and flow-controlled (StreamWindow per direction); datagrams
// are at most MaxDatagram bytes, and a full receive queue drops, as a
// socket buffer would.
type Local struct {
	prefix netip.Prefix

	mu     sync.Mutex
	next   netip.Addr
	names  map[string]netip.Addr // kept across detach: a re-attach gets the same address
	byAddr map[netip.Addr]*localEP
	tcp    map[netip.AddrPort]*localListener
	udp    map[netip.AddrPort]*udpSock
	eph    uint16 // the next client port
	closed bool
	// aliases are the attached endpoints by alias (AttachAliases); rr
	// rotates each alias's answer.
	aliases map[string]map[*localEP]struct{}
	rr      map[string]uint32
}

const (
	// MaxDatagram bounds one datagram (the relay's limit).
	MaxDatagram = 8 << 10
	// DatagramQueue is how many datagrams a socket holds before dropping.
	DatagramQueue = 256
	// Backlog is how many connections wait for Accept before a connect is
	// refused.
	Backlog = 128
	// MaxEndpoints bounds a network's containers.
	MaxEndpoints = 4096
	// MaxSockets bounds the listeners and datagram sockets of one network.
	MaxSockets = 1 << 15
	// MaxAliases bounds one endpoint's aliases.
	MaxAliases = 32

	ephemeralFirst = 49152
)

// DefaultPrefix is Local's network unless NewLocal is given another.
var DefaultPrefix = netip.MustParsePrefix("10.88.0.0/16")

// NewLocal returns an empty network on prefix (an IPv4 prefix of at least
// 2 host bits).
func NewLocal(prefix netip.Prefix) (*Local, error) {
	prefix = prefix.Masked()
	if !prefix.Addr().Is4() || prefix.Bits() > 30 {
		return nil, fmt.Errorf("netdriver: %s: want an IPv4 prefix of /30 or wider", prefix)
	}
	return &Local{
		prefix:  prefix,
		next:    prefix.Addr().Next().Next(), // .1 is the gateway's, by habit
		names:   map[string]netip.Addr{},
		byAddr:  map[netip.Addr]*localEP{},
		tcp:     map[netip.AddrPort]*localListener{},
		udp:     map[netip.AddrPort]*udpSock{},
		eph:     ephemeralFirst,
		aliases: map[string]map[*localEP]struct{}{},
		rr:      map[string]uint32{},
	}, nil
}

// Prefix is the network's address range.
func (n *Local) Prefix() netip.Prefix { return n.prefix }

// Attach implements NetworkDriver.
func (n *Local) Attach(ctx context.Context, container string) (Endpoint, error) {
	return n.AttachAliases(ctx, container, nil)
}

// AttachAliases implements AliasDriver: the container also answers to
// aliases on this network (Docker's network aliases; a compose service's
// name). An alias several containers share resolves to all of them, in an
// order that rotates from one query to the next. A container's own name
// wins over another's alias. Attaching again replaces the aliases.
func (n *Local) AttachAliases(ctx context.Context, container string, aliases []string) (Endpoint, error) {
	if !ValidName(container) {
		return nil, fmt.Errorf("netdriver: %q is not a DNS label", container)
	}
	if len(aliases) > MaxAliases {
		return nil, fmt.Errorf("netdriver: at most %d aliases", MaxAliases)
	}
	for _, a := range aliases {
		if !ValidName(a) {
			return nil, fmt.Errorf("netdriver: alias %q is not a DNS label", a)
		}
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	ep, err := n.attachLocked(container)
	if err != nil {
		return nil, err
	}
	n.setAliasesLocked(ep, aliases)
	return ep, nil
}

// setAliasesLocked replaces ep's aliases; the caller holds n.mu.
func (n *Local) setAliasesLocked(ep *localEP, aliases []string) {
	for _, a := range ep.aliases {
		if set := n.aliases[a]; set != nil {
			delete(set, ep)
			if len(set) == 0 {
				delete(n.aliases, a)
				delete(n.rr, a)
			}
		}
	}
	ep.aliases = nil
	for _, a := range aliases {
		if a == ep.name || slices.Contains(ep.aliases, a) {
			continue
		}
		ep.aliases = append(ep.aliases, a)
		set := n.aliases[a]
		if set == nil {
			set = map[*localEP]struct{}{}
			n.aliases[a] = set
		}
		set[ep] = struct{}{}
	}
}

func (n *Local) attachLocked(container string) (*localEP, error) {
	if n.closed {
		return nil, net.ErrClosed
	}
	addr, ok := n.names[container]
	if !ok {
		if len(n.names) >= MaxEndpoints {
			return nil, syscall.ENOBUFS
		}
		for n.byAddr[n.next] != nil || n.taken(n.next) {
			n.next = n.next.Next()
		}
		if !n.prefix.Contains(n.next.Next()) { // keep the broadcast address
			return nil, fmt.Errorf("netdriver: %s is full", n.prefix)
		}
		addr = n.next
		n.next = n.next.Next()
		n.names[container] = addr
	}
	if old := n.byAddr[addr]; old != nil {
		return old, nil
	}
	ep := &localEP{n: n, name: container, addr: addr}
	n.byAddr[addr] = ep
	return ep, nil
}

func (n *Local) taken(a netip.Addr) bool {
	for _, v := range n.names {
		if v == a {
			return true
		}
	}
	return false
}

// Close detaches every container.
func (n *Local) Close() error {
	n.mu.Lock()
	eps := make([]*localEP, 0, len(n.byAddr))
	for _, ep := range n.byAddr {
		eps = append(eps, ep)
	}
	n.closed = true
	n.mu.Unlock()
	for _, ep := range eps {
		ep.Detach()
	}
	return nil
}

type localEP struct {
	n       *Local
	name    string
	addr    netip.Addr
	aliases []string // guarded by n.mu
}

func (e *localEP) Addr() netip.Addr { return e.addr }

// Prefix is the endpoint's network (Multi routes a dial by it).
func (e *localEP) Prefix() netip.Prefix { return e.n.prefix }

// own maps the loopback address and the unspecified one to the
// endpoint's own.
func (e *localEP) own(a netip.Addr) netip.Addr {
	if a.IsLoopback() || a.IsUnspecified() {
		return e.addr
	}
	return a
}

func (e *localEP) attached() bool { return e.n.byAddr[e.addr] == e }

// freePort finds an ephemeral port free on addr for the map's protocol;
// the caller holds n.mu.
func freePort[T any](m map[netip.AddrPort]T, addr netip.Addr) (uint16, bool) {
	for p := ephemeralFirst; p <= 65535; p++ {
		if _, used := m[netip.AddrPortFrom(addr, uint16(p))]; !used {
			return uint16(p), true
		}
	}
	return 0, false
}

func (e *localEP) Listen(ctx context.Context, network string, port uint16) (net.Listener, error) {
	if network != "tcp" {
		return nil, syscall.EPROTOTYPE
	}
	n := e.n
	n.mu.Lock()
	defer n.mu.Unlock()
	if !e.attached() {
		return nil, syscall.ENETDOWN
	}
	if len(n.tcp)+len(n.udp) >= MaxSockets {
		return nil, syscall.ENOBUFS
	}
	if port == 0 {
		p, ok := freePort(n.tcp, e.addr)
		if !ok {
			return nil, syscall.EADDRINUSE
		}
		port = p
	}
	ap := netip.AddrPortFrom(e.addr, port)
	if n.tcp[ap] != nil {
		return nil, syscall.EADDRINUSE
	}
	l := &localListener{n: n, ap: ap, queue: make(chan *pipeConn, Backlog), done: make(chan struct{})}
	n.tcp[ap] = l
	return l, nil
}

func (e *localEP) Dial(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error) {
	dst := netip.AddrPortFrom(e.own(addr.Addr().Unmap()), addr.Port())
	n := e.n
	switch network {
	case "tcp":
		n.mu.Lock()
		if !e.attached() {
			n.mu.Unlock()
			return nil, syscall.ENETDOWN
		}
		if n.byAddr[dst.Addr()] == nil {
			n.mu.Unlock()
			return nil, syscall.EHOSTUNREACH
		}
		l := n.tcp[dst]
		if l == nil {
			n.mu.Unlock()
			return nil, syscall.ECONNREFUSED
		}
		n.eph++
		if n.eph < ephemeralFirst {
			n.eph = ephemeralFirst
		}
		src := netip.AddrPortFrom(e.addr, n.eph)
		n.mu.Unlock()
		client, server := streamPair(net.TCPAddrFromAddrPort(src), net.TCPAddrFromAddrPort(dst))
		select {
		case l.queue <- server:
			return client, nil
		case <-l.done:
			return nil, syscall.ECONNREFUSED
		default:
			return nil, syscall.ECONNREFUSED // backlog full
		}
	case "udp":
		pc, err := e.ListenPacket(ctx, "udp", 0)
		if err != nil {
			return nil, err
		}
		return &udpConn{udpSock: pc.(*udpSock), peer: dst}, nil
	}
	return nil, syscall.EPROTOTYPE
}

func (e *localEP) ListenPacket(ctx context.Context, network string, port uint16) (net.PacketConn, error) {
	if network != "udp" {
		return nil, syscall.EPROTOTYPE
	}
	n := e.n
	n.mu.Lock()
	defer n.mu.Unlock()
	if !e.attached() {
		return nil, syscall.ENETDOWN
	}
	if len(n.tcp)+len(n.udp) >= MaxSockets {
		return nil, syscall.ENOBUFS
	}
	if port == 0 {
		p, ok := freePort(n.udp, e.addr)
		if !ok {
			return nil, syscall.EADDRINUSE
		}
		port = p
	}
	ap := netip.AddrPortFrom(e.addr, port)
	if n.udp[ap] != nil {
		return nil, syscall.EADDRINUSE
	}
	s := &udpSock{n: n, ap: ap, notify: make(chan struct{})}
	n.udp[ap] = s
	return s, nil
}

func (e *localEP) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(name); err == nil {
		return []netip.Addr{a}, nil
	}
	if name == "localhost" {
		return []netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1})}, nil
	}
	key := strings.ToLower(strings.TrimSuffix(name, "."))
	n := e.n
	n.mu.Lock()
	defer n.mu.Unlock()
	if a, ok := n.names[key]; ok && n.byAddr[a] != nil {
		return []netip.Addr{a}, nil
	}
	if set := n.aliases[key]; len(set) > 0 {
		out := make([]netip.Addr, 0, len(set))
		for ep := range set {
			out = append(out, ep.addr)
		}
		slices.SortFunc(out, netip.Addr.Compare)
		r := int(n.rr[key] % uint32(len(out)))
		n.rr[key]++
		return append(out[r:], out[:r]...), nil
	}
	return nil, &net.DNSError{Err: "no such container", Name: name, IsNotFound: true}
}

// Detach closes the endpoint's listeners and datagram sockets and takes
// its address off the network (a later Attach of the name gets it back).
func (e *localEP) Detach() error {
	n := e.n
	n.mu.Lock()
	if n.byAddr[e.addr] != e {
		n.mu.Unlock()
		return nil
	}
	delete(n.byAddr, e.addr)
	n.setAliasesLocked(e, nil)
	var ls []*localListener
	var us []*udpSock
	for ap, l := range n.tcp {
		if ap.Addr() == e.addr {
			ls = append(ls, l)
		}
	}
	for ap, s := range n.udp {
		if ap.Addr() == e.addr {
			us = append(us, s)
		}
	}
	n.mu.Unlock()
	for _, l := range ls {
		l.Close()
	}
	for _, s := range us {
		s.Close()
	}
	return nil
}

type localListener struct {
	n     *Local
	ap    netip.AddrPort
	queue chan *pipeConn
	done  chan struct{}
	once  sync.Once
}

func (l *localListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.queue:
		return c, nil
	case <-l.done:
		return nil, &net.OpError{Op: "accept", Net: "tcp", Addr: l.Addr(), Err: net.ErrClosed}
	}
}

func (l *localListener) Close() error {
	l.once.Do(func() {
		l.n.mu.Lock()
		if l.n.tcp[l.ap] == l {
			delete(l.n.tcp, l.ap)
		}
		l.n.mu.Unlock()
		close(l.done)
		for {
			select {
			case c := <-l.queue:
				c.Reset() // never accepted: the caller sees a reset
			default:
				return
			}
		}
	})
	return nil
}

func (l *localListener) Addr() net.Addr { return net.TCPAddrFromAddrPort(l.ap) }

// udpSock is a bound datagram socket.
type udpSock struct {
	n  *Local
	ap netip.AddrPort

	mu       sync.Mutex
	queue    []datagram
	closed   bool
	deadline netDeadline
	notify   chan struct{}
}

type datagram struct {
	from netip.AddrPort
	b    []byte
}

type netDeadline struct{ r, w timeT }

func (s *udpSock) changed() {
	close(s.notify)
	s.notify = make(chan struct{})
}

// deliver queues b for the socket bound at dst, or drops it.
func (n *Local) deliver(from, dst netip.AddrPort, b []byte) {
	n.mu.Lock()
	s := n.udp[dst]
	n.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || len(s.queue) >= DatagramQueue {
		return
	}
	s.queue = append(s.queue, datagram{from, append([]byte(nil), b...)})
	s.changed()
}

func (s *udpSock) readFrom(b []byte, filter func(netip.AddrPort) bool) (int, netip.AddrPort, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for {
		if s.closed {
			return 0, netip.AddrPort{}, net.ErrClosed
		}
		if past(s.deadline.r) {
			return 0, netip.AddrPort{}, os.ErrDeadlineExceeded
		}
		for i, d := range s.queue {
			if filter != nil && !filter(d.from) {
				continue
			}
			s.queue = append(s.queue[:i], s.queue[i+1:]...)
			return copy(b, d.b), d.from, nil
		}
		ch := s.notify
		dl := s.deadline.r
		s.mu.Unlock()
		err := waitUntil(ch, dl)
		s.mu.Lock()
		if err != nil {
			return 0, netip.AddrPort{}, err
		}
	}
}

func (s *udpSock) ReadFrom(b []byte) (int, net.Addr, error) {
	k, from, err := s.readFrom(b, nil)
	if err != nil {
		return 0, nil, &net.OpError{Op: "read", Net: "udp", Source: s.LocalAddr(), Err: err}
	}
	return k, net.UDPAddrFromAddrPort(from), nil
}

func (s *udpSock) writeTo(b []byte, dst netip.AddrPort) (int, error) {
	if len(b) > MaxDatagram {
		return 0, syscall.EMSGSIZE
	}
	s.mu.Lock()
	closed := s.closed
	s.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	if dst.Addr().Unmap().IsLoopback() {
		dst = netip.AddrPortFrom(s.ap.Addr(), dst.Port())
	}
	s.n.deliver(s.ap, netip.AddrPortFrom(dst.Addr().Unmap(), dst.Port()), b)
	return len(b), nil
}

func (s *udpSock) WriteTo(b []byte, addr net.Addr) (int, error) {
	ua, ok := addr.(*net.UDPAddr)
	if !ok {
		return 0, &net.OpError{Op: "write", Net: "udp", Err: net.InvalidAddrError("not a UDP address")}
	}
	k, err := s.writeTo(b, ua.AddrPort())
	if err != nil {
		return 0, &net.OpError{Op: "write", Net: "udp", Source: s.LocalAddr(), Addr: addr, Err: err}
	}
	return k, nil
}

func (s *udpSock) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	s.queue = nil
	s.changed()
	s.mu.Unlock()
	s.n.mu.Lock()
	if s.n.udp[s.ap] == s {
		delete(s.n.udp, s.ap)
	}
	s.n.mu.Unlock()
	return nil
}

func (s *udpSock) LocalAddr() net.Addr { return net.UDPAddrFromAddrPort(s.ap) }

func (s *udpSock) SetDeadline(t timeT) error {
	s.SetReadDeadline(t)
	return s.SetWriteDeadline(t)
}

func (s *udpSock) SetReadDeadline(t timeT) error {
	s.mu.Lock()
	s.deadline.r = t
	s.changed()
	s.mu.Unlock()
	return nil
}

// SetWriteDeadline is accepted: a datagram write never blocks.
func (s *udpSock) SetWriteDeadline(t timeT) error { return nil }

// udpConn is a connected datagram socket: writes go to peer, reads take
// only what peer sent.
type udpConn struct {
	*udpSock
	peer netip.AddrPort
}

func (c *udpConn) Read(b []byte) (int, error) {
	k, _, err := c.readFrom(b, func(from netip.AddrPort) bool { return from == c.peer })
	if err != nil {
		return 0, &net.OpError{Op: "read", Net: "udp", Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: err}
	}
	return k, nil
}

func (c *udpConn) Write(b []byte) (int, error) {
	k, err := c.writeTo(b, c.peer)
	if err != nil {
		return 0, &net.OpError{Op: "write", Net: "udp", Source: c.LocalAddr(), Addr: c.RemoteAddr(), Err: err}
	}
	return k, nil
}

func (c *udpConn) RemoteAddr() net.Addr { return net.UDPAddrFromAddrPort(c.peer) }
