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
