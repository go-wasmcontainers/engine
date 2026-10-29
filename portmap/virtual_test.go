package portmap

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"strings"
	"syscall"
	"testing"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// webContainer is a container on an in-engine network serving HTTP on 80.
func webContainer(t *testing.T) Dialer {
	t.Helper()
	n, err := netdriver.NewLocal(netdriver.DefaultPrefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { n.Close() })
	ep, err := n.Attach(context.Background(), "web")
	if err != nil {
		t.Fatal(err)
	}
	ln, err := ep.Listen(context.Background(), "tcp", 80)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "hello %s %s host=%s", r.Method, r.URL.Path, r.Host)
	})}
	go srv.Serve(ln)
	t.Cleanup(func() { srv.Close() })
	return ep.Dial
}

func TestVirtualHTTP(t *testing.T) {
	v := NewVirtual()
	m, _ := ParseSpecs([]string{"8080:80"})
	s, err := Publish(context.Background(), v, webContainer(t), m, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Ports()[Key{80, "tcp"}][0].HostPort; got != 8080 {
		t.Fatalf("bound %d", got)
	}
	cl := &http.Client{Transport: v.Transport()}
	for i := 0; i < 3; i++ { // a fresh connection each time
		res, err := cl.Get("http://localhost:8080/x")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(res.Body)
		res.Body.Close()
		if string(b) != "hello GET /x host=localhost:8080" {
			t.Fatalf("%q", b)
		}
	}
	// The same host port again: taken, as a socket would be.
	_, err = Publish(context.Background(), v, webContainer(t), m, Limits{})
	if !errors.Is(err, ErrAllocated) {
		t.Fatalf("second binding of 8080: %v", err)
	}
	s.Close()
	if _, err := cl.Get("http://localhost:8080/x"); err == nil || !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("after Close: %v", err)
	}
	if len(v.Ports()) != 0 {
		t.Fatalf("still listening: %v", v.Ports())
	}
}

func TestVirtualListen(t *testing.T) {
	v := NewVirtual()
	ctx := context.Background()
	if _, err := v.ListenPacket(ctx, "udp", netip.AddrPort{}); !errors.Is(err, syscall.EPROTONOSUPPORT) {
		t.Fatalf("udp: %v", err)
	}
	if _, err := v.Listen(ctx, "udp", netip.AddrPort{}); !errors.Is(err, syscall.EPROTONOSUPPORT) {
		t.Fatalf("listen udp: %v", err)
	}
	// Port 0: an ephemeral one, reported by Addr.
	l, err := v.Listen(ctx, "tcp", netip.MustParseAddrPort("127.0.0.1:0"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(l.Addr().String(), "127.0.0.1:49152") {
		t.Fatalf("ephemeral %s", l.Addr())
	}
	l2, _ := v.Listen(ctx, "tcp", netip.MustParseAddrPort("127.0.0.1:0"))
	if l2.Addr().String() == l.Addr().String() {
		t.Fatal("two listeners on one ephemeral port")
	}
	// Nothing accepted: the backlog fills, then a Dial is refused.
	for i := 0; i < VirtualBacklog; i++ {
		if _, err := v.Dial(ctx, 49152); err != nil {
			t.Fatalf("dial %d: %v", i, err)
		}
	}
	if _, err := v.Dial(ctx, 49152); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("over the backlog: %v", err)
	}
	if _, err := v.Dial(ctx, 1); !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("nothing there: %v", err)
	}
	// Close drops the queued connections and refuses Accept.
	l.Close()
	l.Close()
	if _, err := l.Accept(); err == nil {
		t.Fatal("accept after close")
	}
	l2.Close()
	// The Transport refuses a URL without a usable port.
	cl := &http.Client{Transport: v.Transport()}
	if _, err := cl.Get("http://localhost:0/"); err == nil {
		t.Fatal("port 0 dialed")
	}
}
