package network

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver"
)

type hasCloseWrite interface{ CloseWrite() error }
type hasCloseRead interface{ CloseRead() error }

// The conns and packet sockets of an internal network keep every optional
// interface the same sockets have on an ordinary network, and the half-close
// still reaches the peer (shutdown(SHUT_WR) and the port proxy rely on it).
func TestSealedKeepsOptionalInterfaces(t *testing.T) {
	ctx := ctx5(t)
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("sealed", "relay", localDriver(t), true); err != nil {
		t.Fatal(err)
	}
	type shape struct{ cw, cr, pc bool }
	probe := func(c net.Conn) shape {
		_, cw := c.(hasCloseWrite)
		_, cr := c.(hasCloseRead)
		_, pc := c.(net.PacketConn)
		return shape{cw, cr, pc}
	}
	var got [2][2]shape // network x (tcp, udp)
	for i, n := range []string{"bridge", "sealed"} {
		a, err := m.Attach(ctx, n, "a", nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := m.Attach(ctx, n, "b", nil)
		if err != nil {
			t.Fatal(err)
		}
		l, err := b.Listen(ctx, "tcp", 80)
		if err != nil {
			t.Fatal(err)
		}
		c, err := a.Dial(ctx, "tcp", netip.AddrPortFrom(b.Addr(), 80))
		if err != nil {
			t.Fatal(err)
		}
		pk, err := b.ListenPacket(ctx, "udp", 53)
		if err != nil {
			t.Fatal(err)
		}
		u, err := a.Dial(ctx, "udp", netip.AddrPortFrom(b.Addr(), 53))
		if err != nil {
			t.Fatal(err)
		}
		got[i][0], got[i][1] = probe(c), probe(u)
		if n == "sealed" {
			// The half-close reaches the peer, which reads EOF.
			sc, err := l.Accept()
			if err != nil {
				t.Fatal(err)
			}
			if err := c.(hasCloseWrite).CloseWrite(); err != nil {
				t.Fatal(err)
			}
			sc.SetReadDeadline(time.Now().Add(2 * time.Second))
			if _, err := sc.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("peer after CloseWrite: %v", err)
			}
			sc.Close()
		}
		c.Close()
		u.Close()
		l.Close()
		pk.Close()
	}
	if got[0] != got[1] {
		t.Fatalf("optional interfaces differ: ordinary %+v, internal %+v", got[0], got[1])
	}
	if !got[0][0].cw || !got[0][1].pc {
		t.Fatalf("the ordinary network has no CloseWrite or no PacketConn: %+v", got[0])
	}
}

// An internal-network driver held by value, with a field that cannot be
// hashed, opens sockets without panicking the daemon.
type sliceDriver struct {
	*netdriver.Local
	tags []string
}

func (sliceDriver) EgressEnabled() bool { return false }

func TestSealedUnhashableDriver(t *testing.T) {
	ctx := ctx5(t)
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("sl", "relay", sliceDriver{localDriver(t), []string{"a"}}, true); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Attach(ctx, "sl", "c1", nil)
	if err != nil {
		t.Fatal(err)
	}
	p, err := ep.ListenPacket(ctx, "udp", 0)
	if err != nil {
		t.Fatal(err)
	}
	l, err := ep.Listen(ctx, "tcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	p.Close()
	l.Close()
}

// notifyDriver is a driver that says when it starts to carry traffic out.
type notifyState struct {
	on  atomic.Bool
	mu  sync.Mutex
	fns map[int]func()
	n   int
}

type notifyDriver struct {
	*netdriver.Local
	st *notifyState
}

func (d notifyDriver) EgressEnabled() bool { return d.st.on.Load() }

func (d notifyDriver) NotifyEgress(fn func()) func() {
	d.st.mu.Lock()
	defer d.st.mu.Unlock()
	if d.st.fns == nil {
		d.st.fns = map[int]func(){}
	}
	d.st.n++
	id := d.st.n
	d.st.fns[id] = fn
	return func() {
		d.st.mu.Lock()
		delete(d.st.fns, id)
		d.st.mu.Unlock()
	}
}

func (d notifyDriver) flip() {
	d.st.on.Store(true)
	d.st.mu.Lock()
	var fns []func()
	for _, f := range d.st.fns {
		fns = append(fns, f)
	}
	d.st.mu.Unlock()
	for _, f := range fns {
		f()
	}
}

// A driver that notifies has every socket closed at that moment, with no call
// on any of them: the peer sees the close at once.
func TestSealedClosedAtOnceOnNotify(t *testing.T) {
	ctx := ctx5(t)
	loc := localDriver(t)
	d := notifyDriver{loc, &notifyState{}}
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("sealed", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	a, err := m.Attach(ctx, "sealed", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := loc.Attach(ctx, "b") // not sealed: observes the close
	if err != nil {
		t.Fatal(err)
	}
	l, err := peer.Listen(ctx, "tcp", 80)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	c, err := a.Dial(ctx, "tcp", netip.AddrPortFrom(peer.Addr(), 80))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	sc, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer sc.Close()
	d.flip()
	sc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := sc.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) && !isReset(err) {
		t.Fatalf("the peer of a socket on a network that gained egress: %v", err)
	}
	// Detach stops the notification.
	if err := a.Detach(); err != nil {
		t.Fatal(err)
	}
	d.st.mu.Lock()
	left := len(d.st.fns)
	d.st.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d notifications left after Detach", left)
	}
}

func isReset(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && !ne.Timeout()
}

// flipEP turns egress on in the middle of a Dial, after the endpoint's guard
// has passed: the socket it returns must not survive.
type flipEP struct {
	netdriver.Endpoint
	on   *atomic.Bool
	peer *net.Conn
}

func (e flipEP) Dial(context.Context, string, netip.AddrPort) (net.Conn, error) {
	a, b := net.Pipe()
	*e.peer = b
	e.on.Store(true)
	return a, nil
}

type flipDrv struct {
	*netdriver.Local
	on   *atomic.Bool
	peer *net.Conn
}

func (d flipDrv) EgressEnabled() bool { return d.on.Load() }
func (d flipDrv) AttachAliases(ctx context.Context, n string, al []string) (netdriver.Endpoint, error) {
	ep, err := d.Local.AttachAliases(ctx, n, al)
	return flipEP{ep, d.on, d.peer}, err
}
func (d flipDrv) Attach(ctx context.Context, n string) (netdriver.Endpoint, error) {
	ep, err := d.Local.Attach(ctx, n)
	return flipEP{ep, d.on, d.peer}, err
}

// A Dial that passed the guard before the flip is refused when it registers,
// and its socket is closed, not left to outlive the network's closing.
func TestSealedDialRacingTheFlip(t *testing.T) {
	ctx := ctx5(t)
	var peer net.Conn
	d := flipDrv{localDriver(t), &atomic.Bool{}, &peer}
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("sealed", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	a, err := m.Attach(ctx, "sealed", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.Dial(ctx, "tcp", pub)
	if !errors.Is(err, ErrEgress) || c != nil {
		t.Fatalf("Dial that raced the flip: %v, %v", c, err)
	}
	peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
		t.Fatalf("the raced socket was left open: %v", err)
	}
}

func (e flipEP) Listen(ctx context.Context, n string, p uint16) (net.Listener, error) {
	e.on.Store(true)
	return e.Endpoint.Listen(ctx, n, p)
}

func (e flipEP) ListenPacket(ctx context.Context, n string, p uint16) (net.PacketConn, error) {
	e.on.Store(true)
	return e.Endpoint.ListenPacket(ctx, n, p)
}

// Listen and ListenPacket that race the flip are refused and their sockets
// closed: the port can be bound again through the raw endpoint.
func TestSealedListenRacingTheFlip(t *testing.T) {
	ctx := ctx5(t)
	var peer net.Conn
	loc := localDriver(t)
	d := flipDrv{loc, &atomic.Bool{}, &peer}
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("sealed", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	a, err := m.Attach(ctx, "sealed", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := loc.Attach(ctx, "a")
	if err != nil {
		t.Fatal(err)
	}
	if l, err := a.Listen(ctx, "tcp", 80); !errors.Is(err, ErrEgress) || l != nil {
		t.Fatalf("Listen that raced the flip: %v, %v", l, err)
	}
	l, err := raw.Listen(ctx, "tcp", 80)
	if err != nil {
		t.Fatalf("the raced listener was left open: %v", err)
	}
	l.Close()
	d.on.Store(false)
	if p, err := a.ListenPacket(ctx, "udp", 53); !errors.Is(err, ErrEgress) || p != nil {
		t.Fatalf("ListenPacket that raced the flip: %v, %v", p, err)
	}
	p, err := raw.ListenPacket(ctx, "udp", 53)
	if err != nil {
		t.Fatalf("the raced packet socket was left open: %v", err)
	}
	p.Close()
}

// Reads are guarded too, on every kind of socket, and an Accept refused by
// the flip closes the endpoint's other sockets, not only itself.
func TestSealedReadsAndAcceptAfterSilentFlip(t *testing.T) {
	ctx := ctx5(t)
	loc := localDriver(t)
	d := notifyDriver{loc, &notifyState{}}
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("sealed", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	a, err := m.Attach(ctx, "sealed", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	peer, err := loc.Attach(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	pl, _ := peer.Listen(ctx, "tcp", 80)
	defer pl.Close()
	pp, _ := peer.ListenPacket(ctx, "udp", 53)
	defer pp.Close()
	c, err := a.Dial(ctx, "tcp", netip.AddrPortFrom(peer.Addr(), 80))
	if err != nil {
		t.Fatal(err)
	}
	psc, _ := pl.Accept()
	defer psc.Close()
	u, err := a.Dial(ctx, "udp", netip.AddrPortFrom(peer.Addr(), 53))
	if err != nil {
		t.Fatal(err)
	}
	up := u.(net.PacketConn)
	lp, err := a.ListenPacket(ctx, "udp", 5353)
	if err != nil {
		t.Fatal(err)
	}
	al, err := a.Listen(ctx, "tcp", 8080)
	if err != nil {
		t.Fatal(err)
	}
	d.st.on.Store(true) // silently: no notification
	go func() {
		raw, err := loc.Attach(ctx, "z")
		if err == nil {
			if x, err := raw.Dial(ctx, "tcp", netip.AddrPortFrom(a.Addr(), 8080)); err == nil {
				defer x.Close()
				time.Sleep(time.Second)
			}
		}
	}()
	// The refused Accept closes the endpoint's other sockets: the peer of c
	// sees EOF without any call on c.
	if x, err := al.Accept(); !errors.Is(err, ErrEgress) || x != nil {
		t.Fatalf("Accept: %v, %v", x, err)
	}
	psc.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := psc.Read(make([]byte, 1)); !errors.Is(err, io.EOF) && !isReset(err) {
		t.Fatalf("the refused Accept left another socket open: %v", err)
	}
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, ErrEgress) {
		t.Fatalf("Read: %v", err)
	}
	if _, _, err := up.ReadFrom(make([]byte, 1)); !errors.Is(err, ErrEgress) {
		t.Fatalf("PacketConn.ReadFrom of a dialed UDP socket: %v", err)
	}
	if _, err := up.WriteTo([]byte("x"), net.UDPAddrFromAddrPort(netip.AddrPortFrom(peer.Addr(), 53))); !errors.Is(err, ErrEgress) {
		t.Fatalf("PacketConn.WriteTo of a dialed UDP socket: %v", err)
	}
	if _, _, err := lp.ReadFrom(make([]byte, 1)); !errors.Is(err, ErrEgress) {
		t.Fatalf("ReadFrom: %v", err)
	}
}

// lockNotifier takes ONE lock for EgressEnabled and for calling its callbacks,
// as a driver that notifies under its own lock does; fire can send the notice
// before the flag is visible.
type lockNotifier struct {
	*netdriver.Local
	mu  *sync.Mutex
	on  *bool
	fns *[]func()
}

func newLockNotifier(t *testing.T) lockNotifier {
	return lockNotifier{localDriver(t), &sync.Mutex{}, new(bool), &[]func(){}}
}

func (n lockNotifier) EgressEnabled() bool { n.mu.Lock(); defer n.mu.Unlock(); return *n.on }
func (n lockNotifier) NotifyEgress(fn func()) func() {
	n.mu.Lock()
	*n.fns = append(*n.fns, fn)
	n.mu.Unlock()
	return func() {}
}

// The notice arrives before the flag is visible: it is still the verdict.
func TestSealedNoticeBeforeFlag(t *testing.T) {
	ctx := ctx5(t)
	n := newLockNotifier(t)
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("s", "relay", n, true); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Attach(ctx, "s", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := ep.ListenPacket(ctx, "udp", 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, fn := range *n.fns {
		fn()
	}
	// The flag is still false: the inner socket must close all the same.
	inner := pc.(sealedPacket).PacketConn
	deadline := time.Now().Add(2 * time.Second)
	for {
		inner.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
		_, _, err := inner.ReadFrom(make([]byte, 1))
		if errors.Is(err, net.ErrClosed) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the socket was not closed by a notice that came before the flag: %v", err)
		}
	}
	// And the endpoint stays refused, flag or not.
	if _, err := ep.ListenPacket(ctx, "udp", 0); !errors.Is(err, ErrEgress) {
		t.Fatalf("new socket after the notice: %v", err)
	}
}

// A driver that calls the callback under the lock EgressEnabled takes does not
// deadlock, and a Dial registering at the same time does not either.
func TestSealedNoticeUnderDriverLock(t *testing.T) {
	ctx := ctx5(t)
	n := newLockNotifier(t)
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("s", "relay", n, true); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Attach(ctx, "s", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			if p, err := ep.ListenPacket(ctx, "udp", 0); err == nil {
				p.Close()
			}
		}
	}()
	notified := make(chan struct{})
	go func() {
		n.mu.Lock()
		*n.on = true
		for _, fn := range *n.fns {
			fn()
		}
		n.mu.Unlock()
		close(notified)
	}()
	for _, c := range []chan struct{}{done, notified} {
		select {
		case <-c:
		case <-time.After(5 * time.Second):
			t.Fatal("deadlock between the driver's notice and the endpoint")
		}
	}
}
