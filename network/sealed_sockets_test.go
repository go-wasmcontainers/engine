package network

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// sockEP hands out sockets that count what reaches the wire.
type sockEP struct {
	wrote *atomic.Int32
}

type sockConn struct {
	net.Conn
	wrote *atomic.Int32
}

func (c sockConn) Write(b []byte) (int, error) { c.wrote.Add(1); return len(b), nil }

type sockPC struct {
	net.PacketConn
	wrote *atomic.Int32
}

func (c sockPC) WriteTo(b []byte, _ net.Addr) (int, error) { c.wrote.Add(1); return len(b), nil }

type sockLn struct {
	net.Listener
	wrote  *atomic.Int32
	closed *atomic.Bool
}

func (l sockLn) Close() error {
	l.closed.Store(true)
	return l.Listener.Close()
}

func (l sockLn) Accept() (net.Conn, error) {
	if l.closed.Load() {
		return nil, net.ErrClosed
	}
	a, _ := net.Pipe()
	return sockConn{a, l.wrote}, nil
}

func (sockEP) Addr() netip.Addr { return netip.MustParseAddr("10.9.9.9") }
func (e sockEP) Listen(context.Context, string, uint16) (net.Listener, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return sockLn{l, e.wrote, &atomic.Bool{}}, nil
}
func (e sockEP) Dial(context.Context, string, netip.AddrPort) (net.Conn, error) {
	a, _ := net.Pipe()
	return sockConn{a, e.wrote}, nil
}
func (e sockEP) ListenPacket(context.Context, string, uint16) (net.PacketConn, error) {
	p, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	return sockPC{p, e.wrote}, nil
}
func (sockEP) Resolve(context.Context, string) ([]netip.Addr, error) { return nil, nil }
func (sockEP) Detach() error                                         { return nil }

type sockDrv struct {
	on    *atomic.Bool
	wrote *atomic.Int32
	dead  bool
}

// deadEP fails every socket it is asked for.
type deadEP struct{ sockEP }

func (deadEP) Listen(context.Context, string, uint16) (net.Listener, error) {
	return nil, errors.New("dead")
}
func (deadEP) Dial(context.Context, string, netip.AddrPort) (net.Conn, error) {
	return nil, errors.New("dead")
}
func (deadEP) ListenPacket(context.Context, string, uint16) (net.PacketConn, error) {
	return nil, errors.New("dead")
}

func (d sockDrv) Attach(context.Context, string) (netdriver.Endpoint, error) {
	if d.dead {
		return deadEP{sockEP{d.wrote}}, nil
	}
	return sockEP{d.wrote}, nil
}

// A listener that was closed reports its own error from Accept.
func TestSealedAcceptClosed(t *testing.T) {
	m, _, _ := New(Options{})
	defer m.Close()
	d := sockDrv{&atomic.Bool{}, &atomic.Int32{}, false}
	if _, err := m.AddDriverWith("sealed", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Attach(ctx5(t), "sealed", "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ep.Listen(ctx5(t), "tcp", 80)
	if err != nil {
		t.Fatal(err)
	}
	ln.Close()
	if c, err := ln.Accept(); err == nil || c != nil || errors.Is(err, ErrEgress) {
		t.Fatalf("Accept on a closed listener: %v, %v", c, err)
	}
}

// A peer accepted after the flip is refused, by the accept itself.
func TestSealedAcceptAfterFlip(t *testing.T) {
	m, _, _ := New(Options{})
	defer m.Close()
	d := sockDrv{&atomic.Bool{}, &atomic.Int32{}, false}
	if _, err := m.AddDriverWith("sealed", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Attach(ctx5(t), "sealed", "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ep.Listen(ctx5(t), "tcp", 80)
	if err != nil {
		t.Fatal(err)
	}
	d.on.Store(true)
	if c, err := ln.Accept(); !errors.Is(err, ErrEgress) || c != nil {
		t.Fatalf("Accept after the flip: %v, %v", c, err)
	}
}

// A socket the driver fails to open is that error, not a wrapper around nil.
func TestSealedPassesDriverErrors(t *testing.T) {
	m, _, _ := New(Options{})
	defer m.Close()
	d := sockDrv{&atomic.Bool{}, &atomic.Int32{}, true}
	if _, err := m.AddDriverWith("sealed", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Attach(ctx5(t), "sealed", "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	if c, err := ep.Dial(ctx5(t), "tcp", pub); err == nil || c != nil || errors.Is(err, ErrEgress) {
		t.Fatalf("Dial: %v, %v", c, err)
	}
	if l, err := ep.Listen(ctx5(t), "tcp", 1); err == nil || l != nil {
		t.Fatalf("Listen: %v, %v", l, err)
	}
	if p, err := ep.ListenPacket(ctx5(t), "udp", 1); err == nil || p != nil {
		t.Fatalf("ListenPacket: %v, %v", p, err)
	}
}
func (d sockDrv) Close() error        { return nil }
func (d sockDrv) EgressEnabled() bool { return d.on.Load() }

// Sockets opened before the network learns egress do not carry traffic out
// afterwards: every write is refused and the sockets are closed.
func TestSealedSocketsOutliveNothing(t *testing.T) {
	m, _, _ := New(Options{})
	defer m.Close()
	d := sockDrv{&atomic.Bool{}, &atomic.Int32{}, false}
	if _, err := m.AddDriverWith("sealed", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	ep, err := m.Attach(ctx5(t), "sealed", "web", nil)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := ep.Dial(ctx5(t), "tcp", pub)
	if err != nil {
		t.Fatal(err)
	}
	pc, err := ep.ListenPacket(ctx5(t), "udp", 53)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ep.Listen(ctx5(t), "tcp", 80)
	if err != nil {
		t.Fatal(err)
	}
	acc, err := ln.Accept() // accepted before the flip
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("write while confined: %v", err)
	}
	d.on.Store(true) // the Welcome arrives
	if _, err := conn.Write([]byte("x")); !errors.Is(err, ErrEgress) {
		t.Fatalf("TCP Write after the flip: %v", err)
	}
	if _, err := pc.WriteTo([]byte("x"), net.UDPAddrFromAddrPort(netip.MustParseAddrPort("1.1.1.1:53"))); !errors.Is(err, ErrEgress) {
		t.Fatalf("UDP WriteTo after the flip: %v", err)
	}
	if _, err := acc.Write([]byte("x")); !errors.Is(err, ErrEgress) {
		t.Fatalf("accepted conn Write after the flip: %v", err)
	}
	if c, err := ln.Accept(); err == nil {
		c.Close()
		t.Fatal("Accept after the flip")
	}
	if got := d.wrote.Load(); got != 1 {
		t.Fatalf("%d writes reached the wire, want only the first", got)
	}
	// Every socket was closed: a read fails at once.
	conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(make([]byte, 1)); err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the socket is still open: %v", err)
	}
	if _, _, err := pc.ReadFrom(make([]byte, 1)); err == nil {
		t.Fatal("the packet socket is still open")
	}
}
