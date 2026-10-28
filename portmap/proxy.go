package portmap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"sync"
	"syscall"
	"time"
)

// Host is where published ports listen: the machine's sockets for wctrd
// (OS), something the page provides in a browser.
type Host interface {
	Listen(ctx context.Context, network string, addr netip.AddrPort) (net.Listener, error)
	ListenPacket(ctx context.Context, network string, addr netip.AddrPort) (net.PacketConn, error)
}

// OS is the machine's own sockets.
type OS struct{}

func (OS) Listen(ctx context.Context, network string, addr netip.AddrPort) (net.Listener, error) {
	var lc net.ListenConfig
	return lc.Listen(ctx, network, addr.String())
}

func (OS) ListenPacket(ctx context.Context, network string, addr netip.AddrPort) (net.PacketConn, error) {
	var lc net.ListenConfig
	return lc.ListenPacket(ctx, network, addr.String())
}

// Dialer reaches the container: its own endpoint, dialing 127.0.0.1, so a
// port is reached on whichever of its networks it listens.
type Dialer func(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error)

// Limits bound what one published port may hold.
type Limits struct {
	// MaxConns is the connections one TCP binding proxies at once; the next
	// is closed as soon as it is accepted.
	MaxConns int
	// MaxSessions is the UDP clients one binding keeps at once; a datagram
	// from another is dropped, as a full socket buffer drops.
	MaxSessions int
	// UDPIdle ends a UDP client's session after this long without traffic.
	UDPIdle time.Duration
	// DialTimeout bounds reaching the container.
	DialTimeout time.Duration
}

// DefaultLimits are Docker's UDP timeout (90 s) and bounds sized for a
// classroom, not a production load balancer.
var DefaultLimits = Limits{MaxConns: 128, MaxSessions: 256, UDPIdle: 90 * time.Second, DialTimeout: 5 * time.Second}

func (l Limits) orDefault() Limits {
	d := DefaultLimits
	if l.MaxConns > 0 {
		d.MaxConns = l.MaxConns
	}
	if l.MaxSessions > 0 {
		d.MaxSessions = l.MaxSessions
	}
	if l.UDPIdle > 0 {
		d.UDPIdle = l.UDPIdle
	}
	if l.DialTimeout > 0 {
		d.DialTimeout = l.DialTimeout
	}
	return d
}

// ErrAllocated is a host port somebody else holds.
var ErrAllocated = errors.New("port is already allocated")

// Set is one container's published ports while it runs.
type Set struct {
	mu      sync.Mutex
	actual  Map
	closers []io.Closer
	closed  bool
	wg      sync.WaitGroup
}

// Publish opens every binding of m and proxies it to the container, or
// opens none: a binding that cannot be had closes the others and says
// which ("Bind for 127.0.0.1:8080 failed: port is already allocated", as
// Docker words it). A port with no binding is only exposed.
func Publish(ctx context.Context, host Host, dial Dialer, m Map, lim Limits) (*Set, error) {
	if m.Bindings() > MaxBindings {
		return nil, fmt.Errorf("%w: at most %d published ports", ErrInvalid, MaxBindings)
	}
	lim = lim.orDefault()
	s := &Set{actual: Map{}}
	for _, k := range m.Keys() {
		s.actual[k] = nil
		for _, b := range m[k] {
			got, err := s.open(ctx, host, dial, k, b, lim)
			if err != nil {
				s.Close()
				return nil, err
			}
			s.actual[k] = append(s.actual[k], got)
		}
	}
	return s, nil
}

// open listens for one binding, trying each port of a range in turn.
func (s *Set) open(ctx context.Context, host Host, dial Dialer, k Key, b Binding, lim Limits) (Binding, error) {
	lo, hi := b.HostPort, b.HostPortEnd
	if hi < lo {
		hi = lo
	}
	var lastErr error
	for p := int(lo); p <= int(hi); p++ {
		ap := netip.AddrPortFrom(b.Addr(), uint16(p))
		got, err := s.listen(ctx, host, dial, k, ap, lim)
		if err == nil {
			return got, nil
		}
		lastErr = err
		if !errors.Is(err, syscall.EADDRINUSE) {
			break
		}
	}
	shown := b
	if errors.Is(lastErr, syscall.EADDRINUSE) {
		return Binding{}, fmt.Errorf("Bind for %s failed: %w", shown, ErrAllocated)
	}
	return Binding{}, fmt.Errorf("Bind for %s failed: %v", shown, lastErr)
}

func (s *Set) listen(ctx context.Context, host Host, dial Dialer, k Key, ap netip.AddrPort, lim Limits) (Binding, error) {
	target := netip.AddrPortFrom(netip.AddrFrom4([4]byte{127, 0, 0, 1}), k.Port)
	switch k.Proto {
	case "tcp":
		ln, err := host.Listen(ctx, "tcp", ap)
		if err != nil {
			return Binding{}, err
		}
		got := boundAt(ln.Addr(), ap)
		t := &tcpProxy{ln: ln, dial: dial, target: target, lim: lim, conns: map[net.Conn]struct{}{}, ups: map[net.Conn]struct{}{}}
		s.add(t)
		s.wg.Add(1)
		go func() { defer s.wg.Done(); t.serve() }()
		return got, nil
	case "udp":
		pc, err := host.ListenPacket(ctx, "udp", ap)
		if err != nil {
			return Binding{}, err
		}
		got := boundAt(pc.LocalAddr(), ap)
		u := &udpProxy{pc: pc, dial: dial, target: target, lim: lim, sessions: map[string]*udpSession{}}
		s.add(u)
		s.wg.Add(1)
		go func() { defer s.wg.Done(); u.serve() }()
		return got, nil
	}
	return Binding{}, fmt.Errorf("%w: invalid proto: %s", ErrInvalid, k.Proto)
}

// boundAt is the binding a listener ended up on (the port the system
// picked for a 0).
func boundAt(a net.Addr, asked netip.AddrPort) Binding {
	ap := asked
	switch v := a.(type) {
	case *net.TCPAddr:
		ap = netip.AddrPortFrom(asked.Addr(), uint16(v.Port))
	case *net.UDPAddr:
		ap = netip.AddrPortFrom(asked.Addr(), uint16(v.Port))
	default:
		if p, err := netip.ParseAddrPort(a.String()); err == nil {
			ap = netip.AddrPortFrom(asked.Addr(), p.Port())
		}
	}
	return Binding{HostIP: ap.Addr(), HostPort: ap.Port(), HostPortEnd: ap.Port()}
}

func (s *Set) add(c io.Closer) {
	s.mu.Lock()
	s.closers = append(s.closers, c)
	s.mu.Unlock()
}

// Ports are the bindings actually held: a picked port is the one picked.
func (s *Set) Ports() Map {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := Map{}
	for k, bs := range s.actual {
		out[k] = append([]Binding(nil), bs...)
	}
	return out
}

// Close stops listening and ends every proxied connection; it returns once
// nothing it started runs any more.
func (s *Set) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	cs := s.closers
	s.closers = nil
	s.mu.Unlock()
	for i := len(cs) - 1; i >= 0; i-- {
		cs[i].Close()
	}
	s.wg.Wait()
	return nil
}

// tcpProxy accepts on the host and copies each connection to the
// container.
type tcpProxy struct {
	ln     net.Listener
	dial   Dialer
	target netip.AddrPort
	lim    Limits

	mu     sync.Mutex
	conns  map[net.Conn]struct{} // clients
	ups    map[net.Conn]struct{} // their connections to the container
	closed bool
	wg     sync.WaitGroup
}

func (t *tcpProxy) serve() {
	for {
		c, err := t.ln.Accept()
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			t.wg.Wait()
			return
		}
		if !t.track(c) {
			c.Close() // over MaxConns, or closing
			continue
		}
		t.wg.Add(1)
		go func() {
			defer t.wg.Done()
			defer t.untrack(c)
			t.proxy(c)
		}()
	}
}

func (t *tcpProxy) track(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed || len(t.conns) >= t.lim.MaxConns {
		return false
	}
	t.conns[c] = struct{}{}
	return true
}

func (t *tcpProxy) untrack(c net.Conn) {
	t.mu.Lock()
	delete(t.conns, c)
	t.mu.Unlock()
	c.Close()
}

// trackUp keeps a connection to the container, so Close ends it too; it
// does not count against MaxConns, which counts clients.
func (t *tcpProxy) trackUp(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.ups[c] = struct{}{}
	return true
}

func (t *tcpProxy) untrackUp(c net.Conn) {
	t.mu.Lock()
	delete(t.ups, c)
	t.mu.Unlock()
	c.Close()
}

func (t *tcpProxy) proxy(c net.Conn) {
	ctx, cancel := context.WithTimeout(context.Background(), t.lim.DialTimeout)
	up, err := t.dial(ctx, "tcp", t.target)
	cancel()
	if err != nil {
		return // the client sees the connection close: nothing listens there
	}
	if !t.trackUp(up) {
		up.Close()
		return
	}
	defer t.untrackUp(up)
	done := make(chan struct{}, 2)
	go func() { pipe(up, c); done <- struct{}{} }()
	go func() { pipe(c, up); done <- struct{}{} }()
	<-done
	<-done
}

// pipe copies src to dst and then half-closes dst, so a client that sends
// its request and shuts its write side still reads the whole answer.
func pipe(dst, src net.Conn) {
	io.Copy(dst, src)
	if cw, ok := dst.(interface{ CloseWrite() error }); ok {
		cw.CloseWrite()
		return
	}
	dst.Close()
}

func (t *tcpProxy) Close() error {
	t.mu.Lock()
	t.closed = true
	cs := make([]net.Conn, 0, len(t.conns)+len(t.ups))
	for c := range t.conns {
		cs = append(cs, c)
	}
	for c := range t.ups {
		cs = append(cs, c)
	}
	t.mu.Unlock()
	err := t.ln.Close()
	for _, c := range cs {
		c.Close()
	}
	return err
}

// udpProxy keeps one session per client address: datagrams from the client
// go to the container from one socket, and the container's answers come
// back to that client.
type udpProxy struct {
	pc     net.PacketConn
	dial   Dialer
	target netip.AddrPort
	lim    Limits

	mu       sync.Mutex
	sessions map[string]*udpSession
	closed   bool
	wg       sync.WaitGroup
}

type udpSession struct {
	up   net.Conn
	peer net.Addr
	last time.Time
}

// MaxDatagram bounds one datagram, as the engine's networks do.
const MaxDatagram = 8 << 10

func (u *udpProxy) serve() {
	buf := make([]byte, MaxDatagram+1)
	for {
		n, peer, err := u.pc.ReadFrom(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				continue
			}
			u.wg.Wait()
			return
		}
		if n > MaxDatagram {
			continue // larger than the container's network carries
		}
		s := u.session(peer)
		if s == nil {
			continue
		}
		s.up.Write(buf[:n])
	}
}

// session is peer's session, made on its first datagram; nil when there
// are MaxSessions already or the container cannot be reached.
func (u *udpProxy) session(peer net.Addr) *udpSession {
	key := peer.String()
	u.mu.Lock()
	if s := u.sessions[key]; s != nil {
		s.last = time.Now()
		u.mu.Unlock()
		return s
	}
	if u.closed || len(u.sessions) >= u.lim.MaxSessions {
		u.mu.Unlock()
		return nil
	}
	u.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), u.lim.DialTimeout)
	up, err := u.dial(ctx, "udp", u.target)
	cancel()
	if err != nil {
		return nil
	}
	s := &udpSession{up: up, peer: peer, last: time.Now()}
	u.mu.Lock()
	if u.closed || u.sessions[key] != nil || len(u.sessions) >= u.lim.MaxSessions {
		u.mu.Unlock()
		up.Close()
		return nil
	}
	u.sessions[key] = s
	u.mu.Unlock()
	u.wg.Add(1)
	go func() {
		defer u.wg.Done()
		u.answers(key, s)
	}()
	return s
}

// answers copies the container's datagrams back to the client until the
// session has been idle for UDPIdle.
func (u *udpProxy) answers(key string, s *udpSession) {
	defer func() {
		u.mu.Lock()
		if u.sessions[key] == s {
			delete(u.sessions, key)
		}
		u.mu.Unlock()
		s.up.Close()
	}()
	buf := make([]byte, MaxDatagram)
	for {
		s.up.SetReadDeadline(time.Now().Add(u.lim.UDPIdle))
		n, err := s.up.Read(buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				u.mu.Lock()
				idle := time.Since(s.last) >= u.lim.UDPIdle
				u.mu.Unlock()
				if idle {
					return
				}
				continue
			}
			return
		}
		u.mu.Lock()
		s.last = time.Now()
		u.mu.Unlock()
		u.pc.WriteTo(buf[:n], s.peer)
	}
}

func (u *udpProxy) Close() error {
	u.mu.Lock()
	u.closed = true
	ss := make([]*udpSession, 0, len(u.sessions))
	for _, s := range u.sessions {
		ss = append(ss, s)
	}
	u.mu.Unlock()
	err := u.pc.Close()
	for _, s := range ss {
		s.up.Close()
	}
	return err
}
