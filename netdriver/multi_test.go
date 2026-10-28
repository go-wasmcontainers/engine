package netdriver

import (
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"slices"
	"syscall"
	"testing"
	"time"
)

// TestAliases: an alias shared by replicas answers every one of them, in
// a rotating order; a container's own name wins over another's alias; a
// detached replica leaves the answer; bad aliases are refused.
func TestAliases(t *testing.T) {
	n := newNet(t)
	web1, err := n.AttachAliases(ctx, "web-1", []string{"web", "frontend"})
	if err != nil {
		t.Fatal(err)
	}
	web2, err := n.AttachAliases(ctx, "web-2", []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	client := attach(t, n, "client")
	want := []netip.Addr{web1.Addr(), web2.Addr()}
	slices.SortFunc(want, netip.Addr.Compare)
	firsts := map[netip.Addr]bool{}
	for range 4 {
		got, err := client.Resolve(ctx, "web")
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 {
			t.Fatalf("web = %v, want both replicas", got)
		}
		sorted := slices.Clone(got)
		slices.SortFunc(sorted, netip.Addr.Compare)
		if !slices.Equal(sorted, want) {
			t.Fatalf("web = %v, want %v", got, want)
		}
		firsts[got[0]] = true
	}
	if len(firsts) != 2 {
		t.Errorf("the first answer never rotated: %v", firsts)
	}
	if got, err := client.Resolve(ctx, "FRONTEND."); err != nil || len(got) != 1 || got[0] != web1.Addr() {
		t.Errorf("FRONTEND. = %v, %v", got, err)
	}
	// A container named like another's alias answers for itself.
	own, _ := n.AttachAliases(ctx, "client2", []string{"web-1"})
	if got, _ := client.Resolve(ctx, "web-1"); len(got) != 1 || got[0] != web1.Addr() {
		t.Errorf("web-1 = %v, want the container itself, not %s's alias", got, own.Addr())
	}
	web2.Detach()
	if got, _ := client.Resolve(ctx, "web"); len(got) != 1 || got[0] != web1.Addr() {
		t.Errorf("after detach, web = %v", got)
	}
	web1.Detach()
	if _, err := client.Resolve(ctx, "web"); err == nil {
		t.Error("an alias with no replica left resolved")
	}
	// Attaching again replaces the aliases.
	if _, err := n.AttachAliases(ctx, "web-1", []string{"api"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resolve(ctx, "frontend"); err == nil {
		t.Error("a replaced alias still resolves")
	}
	if got, _ := client.Resolve(ctx, "api"); len(got) != 1 {
		t.Errorf("api = %v", got)
	}
	for _, bad := range [][]string{{"Has_Underscore"}, {""}, {"-x"}, make([]string, MaxAliases+1)} {
		if _, err := n.AttachAliases(ctx, "z", bad); err == nil {
			t.Errorf("aliases %q accepted", bad)
		}
	}
}

func twoNets(t *testing.T) (front, back *Local) {
	t.Helper()
	var err error
	if front, err = NewLocal(netip.MustParsePrefix("10.89.0.0/16")); err != nil {
		t.Fatal(err)
	}
	if back, err = NewLocal(netip.MustParsePrefix("10.90.0.0/16")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { front.Close(); back.Close() })
	return front, back
}

func roundTrip(t *testing.T, from Endpoint, to netip.AddrPort, l net.Listener, msg string) {
	t.Helper()
	c, err := from.Dial(ctx, "tcp", to)
	if err != nil {
		t.Fatalf("dial %s: %v", to, err)
	}
	defer c.Close()
	s, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	go c.Write([]byte(msg))
	b := make([]byte, len(msg))
	if _, err := io.ReadFull(s, b); err != nil || string(b) != msg {
		t.Fatalf("read %q, %v", b, err)
	}
}

// TestMulti: a container on two networks listens on both, reaches each
// network's containers through the right one, gains a network while its
// listener is open, and loses one.
func TestMulti(t *testing.T) {
	front, back := twoNets(t)
	m := NewMulti()
	fe, _ := front.Attach(ctx, "app")
	if err := m.Add(ctx, "front", fe); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(ctx, "front", fe); err == nil {
		t.Error("the same network added twice")
	}
	l, err := m.Listen(ctx, "tcp", 0)
	if err != nil {
		t.Fatal(err)
	}
	port := portOf(l.Addr())
	if port == 0 || m.Addr() != fe.Addr() {
		t.Fatalf("listener %v, addr %v", l.Addr(), m.Addr())
	}
	// Connected while listening: the listener opens on the new network too.
	be, _ := back.Attach(ctx, "app")
	if err := m.Add(ctx, "back", be); err != nil {
		t.Fatal(err)
	}
	fc, _ := front.Attach(ctx, "fclient")
	bc, _ := back.Attach(ctx, "bclient")
	roundTrip(t, fc, netip.AddrPortFrom(fe.Addr(), port), l, "via front")
	roundTrip(t, bc, netip.AddrPortFrom(be.Addr(), port), l, "via back")

	// Dials go out through the network that holds the address.
	bl, _ := bc.Listen(ctx, "tcp", 80)
	fl, _ := fc.Listen(ctx, "tcp", 80)
	roundTrip(t, m, netip.AddrPortFrom(bc.Addr(), 80), bl, "to back")
	roundTrip(t, m, netip.AddrPortFrom(fc.Addr(), 80), fl, "to front")
	// Names resolve on every network.
	for _, name := range []string{"fclient", "bclient"} {
		if got, err := m.Resolve(ctx, name); err != nil || len(got) != 1 {
			t.Errorf("resolve %s = %v, %v", name, got, err)
		}
	}
	if _, err := m.Resolve(ctx, "nobody"); err == nil {
		t.Error("an unknown name resolved")
	}

	// Disconnected: the back network no longer reaches it.
	ep, ok := m.Remove("back")
	if !ok || ep != be {
		t.Fatalf("remove = %v, %v", ep, ok)
	}
	ep.Detach()
	if _, err := bc.Dial(ctx, "tcp", netip.AddrPortFrom(be.Addr(), port)); !errors.Is(err, syscall.EHOSTUNREACH) {
		t.Errorf("dial a disconnected address: %v", err)
	}
	if _, err := m.Dial(ctx, "tcp", netip.AddrPortFrom(bc.Addr(), 80)); err == nil {
		t.Error("dialled a network it left")
	}
	roundTrip(t, fc, netip.AddrPortFrom(fe.Addr(), port), l, "still front")
	if _, ok := m.Remove("back"); ok {
		t.Error("removed twice")
	}
	l.Close()
	if _, err := l.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("accept after close: %v", err)
	}
	m.Detach()
	if _, err := m.Listen(ctx, "tcp", 0); !errors.Is(err, syscall.ENETDOWN) {
		t.Errorf("listen after detach: %v", err)
	}
}

// TestMultiPacket: a datagram socket on two networks receives from both
// and answers each through its own network; the read deadline holds.
func TestMultiPacket(t *testing.T) {
	front, back := twoNets(t)
	m := NewMulti()
	fe, _ := front.Attach(ctx, "dns")
	be, _ := back.Attach(ctx, "dns")
	m.Add(ctx, "front", fe)
	m.Add(ctx, "back", be)
	pc, err := m.ListenPacket(ctx, "udp", 53)
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	for _, side := range []struct {
		n    *Local
		self Endpoint
	}{{front, fe}, {back, be}} {
		cl, _ := side.n.Attach(ctx, "asker")
		c, err := cl.Dial(ctx, "udp", netip.AddrPortFrom(side.self.Addr(), 53))
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("q"))
		b := make([]byte, 16)
		pc.SetReadDeadline(time.Now().Add(5 * time.Second))
		k, from, err := pc.ReadFrom(b)
		if err != nil || string(b[:k]) != "q" {
			t.Fatalf("read %q from %v: %v", b[:k], from, err)
		}
		if _, err := pc.WriteTo([]byte("a"), from); err != nil {
			t.Fatal(err)
		}
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		if k, err := c.Read(b); err != nil || string(b[:k]) != "a" {
			t.Fatalf("answer %q: %v", b[:k], err)
		}
		c.Close()
	}
	pc.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	if _, _, err := pc.ReadFrom(make([]byte, 1)); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("deadline: %v", err)
	}
	// A network added while it is open delivers too.
	third, _ := NewLocal(netip.MustParsePrefix("10.91.0.0/16"))
	defer third.Close()
	te, _ := third.Attach(ctx, "dns")
	m.Add(ctx, "third", te)
	cl, _ := third.Attach(ctx, "late")
	c, _ := cl.Dial(ctx, "udp", netip.AddrPortFrom(te.Addr(), 53))
	c.Write([]byte("late"))
	pc.SetReadDeadline(time.Now().Add(5 * time.Second))
	b := make([]byte, 16)
	if k, _, err := pc.ReadFrom(b); err != nil || string(b[:k]) != "late" {
		t.Errorf("late network: %q, %v", b[:k], err)
	}
}
