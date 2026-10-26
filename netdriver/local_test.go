package netdriver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/nettest"
)

var ctx = context.Background()

func newNet(t *testing.T) *Local {
	t.Helper()
	n, err := NewLocal(DefaultPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	return n
}

func attach(t *testing.T, n *Local, name string) Endpoint {
	t.Helper()
	ep, err := n.Attach(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

// TestConnConformance runs x/net's net.Conn conformance suite (deadlines,
// concurrent reads and writes, close) on a local stream.
func TestConnConformance(t *testing.T) {
	nettest.TestConn(t, func() (c1, c2 net.Conn, stop func(), err error) {
		n, err := NewLocal(DefaultPrefix)
		if err != nil {
			return nil, nil, nil, err
		}
		a, _ := n.Attach(ctx, "a")
		b, _ := n.Attach(ctx, "b")
		l, err := b.Listen(ctx, "tcp", 80)
		if err != nil {
			return nil, nil, nil, err
		}
		c1, err = a.Dial(ctx, "tcp", netip.AddrPortFrom(b.Addr(), 80))
		if err != nil {
			return nil, nil, nil, err
		}
		c2, err = l.Accept()
		if err != nil {
			return nil, nil, nil, err
		}
		return c1, c2, func() { c1.Close(); c2.Close(); n.Close() }, nil
	})
}

func TestStream(t *testing.T) {
	n := newNet(t)
	a, b := attach(t, n, "client"), attach(t, n, "server")
	if a.Addr() == b.Addr() || !n.Prefix().Contains(a.Addr()) {
		t.Fatalf("addresses %s %s", a.Addr(), b.Addr())
	}
	l, err := b.Listen(ctx, "tcp", 8080)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.Listen(ctx, "tcp", 8080); !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("second listen: %v", err)
	}
	ips, err := a.Resolve(ctx, "server")
	if err != nil || len(ips) != 1 || ips[0] != b.Addr() {
		t.Fatalf("resolve: %v %v", ips, err)
	}
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c) // echo until EOF, then close
		c.Close()
	}()
	c, err := a.Dial(ctx, "tcp", netip.AddrPortFrom(ips[0], 8080))
	if err != nil {
		t.Fatal(err)
	}
	if got := c.RemoteAddr().String(); got != b.Addr().String()+":8080" {
		t.Errorf("remote %s", got)
	}
	// More than a window, so the echo has to flow both ways at once.
	msg := bytes.Repeat([]byte("0123456789abcdef"), 3*StreamWindow/16)
	go func() {
		c.Write(msg)
		c.(interface{ CloseWrite() error }).CloseWrite()
	}()
	got, err := io.ReadAll(c)
	if err != nil || !bytes.Equal(got, msg) {
		t.Fatalf("echo: %d bytes, %v", len(got), err)
	}
}

func TestStreamFlowControl(t *testing.T) {
	n := newNet(t)
	a, b := attach(t, n, "a"), attach(t, n, "b")
	l, _ := b.Listen(ctx, "tcp", 1)
	c, err := a.Dial(ctx, "tcp", netip.AddrPortFrom(b.Addr(), 1))
	if err != nil {
		t.Fatal(err)
	}
	s, _ := l.Accept()
	// Nobody reads: a write beyond the window blocks, it does not grow
	// memory.
	c.SetWriteDeadline(time.Now().Add(100 * time.Millisecond))
	k, err := c.Write(make([]byte, 2*StreamWindow))
	if k != StreamWindow || !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("wrote %d, %v", k, err)
	}
	buf := make([]byte, StreamWindow)
	if _, err := io.ReadFull(s, buf); err != nil {
		t.Fatal(err)
	}
	// The peer's close: EPIPE for the writer, EOF for the reader.
	s.Close()
	c.SetWriteDeadline(time.Time{})
	if _, err := c.Write([]byte("x")); !errors.Is(err, syscall.EPIPE) {
		t.Fatalf("write after peer close: %v", err)
	}
	if _, err := c.Read(buf); err != io.EOF {
		t.Fatalf("read after peer close: %v", err)
	}
}

func TestStreamRefusals(t *testing.T) {
	n := newNet(t)
	a, b := attach(t, n, "a"), attach(t, n, "b")
	if _, err := a.Dial(ctx, "tcp", netip.AddrPortFrom(b.Addr(), 9)); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("no listener: %v", err)
	}
	if _, err := a.Dial(ctx, "tcp", netip.MustParseAddrPort("10.88.9.9:80")); !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Errorf("no host: %v", err)
	}
	if _, err := a.Resolve(ctx, "nobody"); err == nil {
		t.Error("resolved nobody")
	} else if d := (*net.DNSError)(nil); !errors.As(err, &d) || !d.IsNotFound {
		t.Errorf("resolve nobody: %v", err)
	}
	// A full backlog refuses; a closed listener resets what it queued.
	l, _ := b.Listen(ctx, "tcp", 80)
	var queued []net.Conn
	for i := 0; i < Backlog; i++ {
		c, err := a.Dial(ctx, "tcp", netip.AddrPortFrom(b.Addr(), 80))
		if err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
		queued = append(queued, c)
	}
	if _, err := a.Dial(ctx, "tcp", netip.AddrPortFrom(b.Addr(), 80)); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Errorf("backlog full: %v", err)
	}
	l.Close()
	if _, err := queued[0].Read(make([]byte, 1)); !errors.Is(err, syscall.ECONNRESET) {
		t.Errorf("queued after close: %v", err)
	}
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("accept after close: %v", err)
	}
}

func TestLoopbackAndEphemeral(t *testing.T) {
	n := newNet(t)
	a := attach(t, n, "solo")
	l, err := a.Listen(ctx, "tcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if port < ephemeralFirst {
		t.Fatalf("ephemeral port %d", port)
	}
	lo, _ := a.Resolve(ctx, "localhost")
	c, err := a.Dial(ctx, "tcp", netip.AddrPortFrom(lo[0], uint16(port)))
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func TestAttachDetach(t *testing.T) {
	n := newNet(t)
	a := attach(t, n, "web")
	if again := attach(t, n, "web"); again.Addr() != a.Addr() {
		t.Fatalf("re-attach: %s, then %s", a.Addr(), again.Addr())
	}
	l, _ := a.Listen(ctx, "tcp", 80)
	a.Detach()
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("listener after detach: %v", err)
	}
	other := attach(t, n, "other")
	if _, err := other.Resolve(ctx, "web"); err == nil {
		t.Error("a detached container still resolves")
	}
	if back := attach(t, n, "web"); back.Addr() != a.Addr() {
		t.Errorf("attach after detach: %s, want %s", back.Addr(), a.Addr())
	}
	for _, bad := range []string{"", "Web", "-a", "a-", "a.b", string(make([]byte, 64))} {
		if _, err := n.Attach(ctx, bad); err == nil {
			t.Errorf("attached %q", bad)
		}
	}
}

func TestDatagrams(t *testing.T) {
	n := newNet(t)
	a, b := attach(t, n, "a"), attach(t, n, "b")
	srv, err := b.ListenPacket(ctx, "udp", 53)
	if err != nil {
		t.Fatal(err)
	}
	c, err := a.Dial(ctx, "udp", netip.AddrPortFrom(b.Addr(), 53))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, MaxDatagram)
	k, from, err := srv.ReadFrom(buf)
	if err != nil || string(buf[:k]) != "ping" || from.String() != c.LocalAddr().String() {
		t.Fatalf("server read %q from %v: %v", buf[:k], from, err)
	}
	srv.WriteTo([]byte("pong"), from)
	c.SetReadDeadline(time.Now().Add(time.Second))
	if k, err := c.Read(buf); err != nil || string(buf[:k]) != "pong" {
		t.Fatalf("client read %q: %v", buf[:k], err)
	}
	if _, err := c.Write(make([]byte, MaxDatagram+1)); !errors.Is(err, syscall.EMSGSIZE) {
		t.Errorf("oversized: %v", err)
	}
	// A full queue drops.
	for i := 0; i < DatagramQueue+10; i++ {
		c.Write([]byte{byte(i)})
	}
	got := 0
	srv.SetReadDeadline(time.Now().Add(50 * time.Millisecond))
	for {
		if _, _, err := srv.ReadFrom(buf); err != nil {
			break
		}
		got++
	}
	if got != DatagramQueue {
		t.Errorf("%d datagrams queued, want %d", got, DatagramQueue)
	}
}

func TestPrefix(t *testing.T) {
	if _, err := NewLocal(netip.MustParsePrefix("fd00::/64")); err == nil {
		t.Error("an IPv6 prefix")
	}
	n, err := NewLocal(netip.MustParsePrefix("192.168.7.0/30"))
	if err != nil {
		t.Fatal(err)
	}
	if ep, err := n.Attach(ctx, "one"); err != nil || ep.Addr().String() != "192.168.7.2" {
		t.Fatalf("first: %v %v", ep, err)
	}
	if _, err := n.Attach(ctx, "two"); err == nil {
		t.Error("a /30 holds one container (network, gateway, one, broadcast)")
	}
}
