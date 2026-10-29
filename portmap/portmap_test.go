package portmap

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver"
)

func TestParseSpec(t *testing.T) {
	lo := DefaultHostIP
	for _, tc := range []struct {
		spec string
		want string // Key=binding;… in Keys order
	}{
		{"80", "80/tcp=127.0.0.1:"},
		{"80/udp", "80/udp=127.0.0.1:"},
		{"80/TCP", "80/tcp=127.0.0.1:"},
		{"8080:80", "80/tcp=127.0.0.1:8080"},
		{"0.0.0.0:8080:80", "80/tcp=0.0.0.0:8080"},
		{"127.0.0.1::80", "80/tcp=127.0.0.1:"},
		{"[::1]:8080:80", "80/tcp=[::1]:8080"},
		{"[::ffff:127.0.0.1]:8080:80", "80/tcp=127.0.0.1:8080"},
		{"8000-8001:80-81", "80/tcp=127.0.0.1:8000;81/tcp=127.0.0.1:8001"},
		{"8000-8010:80", "80/tcp=127.0.0.1:8000-8010"},
		{"80-81", "80/tcp=127.0.0.1:;81/tcp=127.0.0.1:"},
	} {
		m, err := ParseSpec(tc.spec)
		if err != nil {
			t.Errorf("%s: %v", tc.spec, err)
			continue
		}
		var parts []string
		for _, k := range m.Keys() {
			for _, b := range m[k] {
				parts = append(parts, k.String()+"="+b.String())
			}
		}
		if got := strings.Join(parts, ";"); got != tc.want {
			t.Errorf("%s: %s, want %s", tc.spec, got, tc.want)
		}
	}
	_ = lo
}

func TestParseSpecRefuses(t *testing.T) {
	for _, spec := range []string{
		"", "/tcp", "0", "65536", "80/sctp", "80/icmp", "x:80", "8080:x",
		"999.1.1.1:8080:80", "[::1:8080:80", "fe80::1%eth0:8080:80",
		"8000-8001:80-82", "90-80", "1-2000",
	} {
		if _, err := ParseSpec(spec); !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: %v, want ErrInvalid", spec, err)
		}
	}
	if _, err := ParseSpec("80/sctp"); err == nil || !strings.Contains(err.Error(), "tcp and udp only") {
		t.Errorf("sctp: %v", err)
	}
}

func TestParseSpecsAndBinding(t *testing.T) {
	m, err := ParseSpecs([]string{"8080:80", "9090:80", "53/udp"})
	if err != nil {
		t.Fatal(err)
	}
	if len(m[Key{80, "tcp"}]) != 2 || len(m[Key{53, "udp"}]) != 1 || m.Bindings() != 3 {
		t.Fatalf("%v", m)
	}
	if _, err := ParseSpecs([]string{"1-1000", "2000-2100"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("over MaxBindings: %v", err)
	}
	b, err := ParseBinding("", "8000-8010")
	if err != nil || b.HostPort != 8000 || b.HostPortEnd != 8010 || b.Addr() != DefaultHostIP {
		t.Fatalf("%v %v", b, err)
	}
	if b, err = ParseBinding("[::1]", ""); err != nil || b.Addr().String() != "::1" {
		t.Fatalf("%v %v", b, err)
	}
	for _, bad := range [][2]string{{"nope", ""}, {"", "x"}, {"", "0"}} {
		if _, err := ParseBinding(bad[0], bad[1]); !errors.Is(err, ErrInvalid) {
			t.Errorf("%v: %v", bad, err)
		}
	}
	for _, s := range []string{"80", "80/tcp", "53/udp"} {
		if _, err := ParseKey(s); err != nil {
			t.Errorf("%s: %v", s, err)
		}
	}
	if _, err := ParseKey("80/sctp"); err == nil {
		t.Error("sctp key accepted")
	}
}

// container is a container on an in-engine network: an echo server on 80
// (tcp, with a line protocol) and 53 (udp).
func container(t *testing.T) (Dialer, func()) {
	t.Helper()
	n, err := netdriver.NewLocal(netdriver.DefaultPrefix)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := n.Attach(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ep.Listen(context.Background(), "tcp", 80)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if line != "" {
						fmt.Fprintf(c, "echo %s", line)
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	pc, err := ep.ListenPacket(context.Background(), "udp", 53)
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		b := make([]byte, 9000)
		for {
			n, from, err := pc.ReadFrom(b)
			if err != nil {
				return
			}
			pc.WriteTo(append([]byte("echo "), b[:n]...), from)
		}
	}()
	return ep.Dial, func() { ln.Close(); pc.Close(); n.Close() }
}

func tcpAddr(t *testing.T, s *Set, k Key) string {
	t.Helper()
	bs := s.Ports()[k]
	if len(bs) == 0 {
		t.Fatalf("%s not published: %v", k, s.Ports())
	}
	return netip.AddrPortFrom(bs[0].Addr(), bs[0].HostPort).String()
}

func TestPublishTCP(t *testing.T) {
	dial, stop := container(t)
	defer stop()
	m, _ := ParseSpecs([]string{"127.0.0.1::80"})
	s, err := Publish(context.Background(), OS{}, dial, m, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	addr := tcpAddr(t, s, Key{80, "tcp"})
	if strings.HasSuffix(addr, ":0") {
		t.Fatalf("the picked port is not reported: %s", addr)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	// Half-close: the request is sent and the write side shut, and the
	// whole answer still comes back.
	io.WriteString(c, "hello\n")
	c.(*net.TCPConn).CloseWrite()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	got, err := io.ReadAll(c)
	if err != nil || string(got) != "echo hello\n" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestPublishUDP(t *testing.T) {
	dial, stop := container(t)
	defer stop()
	m, _ := ParseSpecs([]string{"127.0.0.1::53/udp"})
	s, err := Publish(context.Background(), OS{}, dial, m, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	bs := s.Ports()[Key{53, "udp"}]
	c, err := net.Dial("udp", netip.AddrPortFrom(bs[0].Addr(), bs[0].HostPort).String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 3; i++ {
		fmt.Fprintf(c, "q%d", i)
		b := make([]byte, 100)
		n, err := c.Read(b)
		if err != nil || string(b[:n]) != fmt.Sprintf("echo q%d", i) {
			t.Fatalf("%d: %q %v", i, b[:n], err)
		}
	}
	// An oversized datagram is dropped, not truncated into the container.
	c.Write(make([]byte, MaxDatagram+1))
	fmt.Fprint(c, "after")
	b := make([]byte, 100)
	n, err := c.Read(b)
	if err != nil || string(b[:n]) != "echo after" {
		t.Fatalf("after an oversized datagram: %q %v", b[:n], err)
	}
}

func TestPublishConflict(t *testing.T) {
	dial, stop := container(t)
	defer stop()
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	port := busy.Addr().(*net.TCPAddr).Port
	// Two bindings, the second taken: nothing stays open.
	m, _ := ParseSpecs([]string{"127.0.0.1::80", fmt.Sprintf("127.0.0.1:%d:80", port)})
	_, err = Publish(context.Background(), OS{}, dial, m, Limits{})
	if !errors.Is(err, ErrAllocated) || !strings.Contains(err.Error(), fmt.Sprintf("Bind for 127.0.0.1:%d failed", port)) {
		t.Fatalf("%v", err)
	}
	// A range skips the port in use.
	m, _ = ParseSpecs([]string{fmt.Sprintf("127.0.0.1:%d-%d:80", port, port+1)})
	s, err := Publish(context.Background(), OS{}, dial, m, Limits{})
	if err != nil {
		t.Skipf("port %d is also taken here: %v", port+1, err)
	}
	defer s.Close()
	if got := s.Ports()[Key{80, "tcp"}][0].HostPort; int(got) != port+1 {
		t.Fatalf("picked %d, want %d", got, port+1)
	}
}

func TestPublishBounds(t *testing.T) {
	dial, stop := container(t)
	defer stop()
	m, _ := ParseSpecs([]string{"127.0.0.1::80"})
	s, err := Publish(context.Background(), OS{}, dial, m, Limits{MaxConns: 1})
	if err != nil {
		t.Fatal(err)
	}
	addr := tcpAddr(t, s, Key{80, "tcp"})
	first, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	io.WriteString(first, "one\n")
	first.SetDeadline(time.Now().Add(5 * time.Second))
	r := bufio.NewReader(first)
	if line, err := r.ReadString('\n'); err != nil || line != "echo one\n" {
		t.Fatalf("%q %v", line, err)
	}
	// The second connection is over MaxConns: closed at once.
	second, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	second.SetDeadline(time.Now().Add(5 * time.Second))
	if n, err := second.Read(make([]byte, 1)); err == nil {
		t.Fatalf("a connection over MaxConns read %d bytes", n)
	}
	second.Close()
	// Close ends the connection still open, and returns.
	done := make(chan struct{})
	go func() { s.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return with a connection open")
	}
	if _, err := r.ReadString('\n'); err == nil {
		t.Fatal("the open connection survived Close")
	}
	if _, err := net.Dial("tcp", addr); err == nil {
		t.Fatal("still listening after Close")
	}
	s.Close() // twice is fine
}

func TestPublishNothingListening(t *testing.T) {
	n, err := netdriver.NewLocal(netdriver.DefaultPrefix)
	if err != nil {
		t.Fatal(err)
	}
	defer n.Close()
	ep, _ := n.Attach(context.Background(), "idle")
	m, _ := ParseSpecs([]string{"127.0.0.1::80"})
	s, err := Publish(context.Background(), OS{}, ep.Dial, m, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	c, err := net.Dial("tcp", tcpAddr(t, s, Key{80, "tcp"}))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("a port nothing listens on answered")
	}
}

func TestExposedOnly(t *testing.T) {
	m := Map{Key{2377, "tcp"}: nil}
	s, err := Publish(context.Background(), OS{}, nil, m, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if bs, ok := s.Ports()[Key{2377, "tcp"}]; !ok || len(bs) != 0 {
		t.Fatalf("%v", s.Ports())
	}
}
