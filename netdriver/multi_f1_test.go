package netdriver

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

// spyRemote is an endpoint on somebody else's machine: it records every name
// it is asked and answers one fixed name.
type spyRemote struct {
	Endpoint
	mu    sync.Mutex
	asked []string
}

func (s *spyRemote) Remote() bool { return true }

func (s *spyRemote) Resolve(_ context.Context, name string) ([]netip.Addr, error) {
	s.mu.Lock()
	s.asked = append(s.asked, name)
	s.mu.Unlock()
	if strings.ToLower(strings.TrimSuffix(name, ".")) == "mate.student" {
		return []netip.Addr{netip.MustParseAddr("10.9.0.7")}, nil
	}
	return nil, &net.DNSError{Err: "no such name", Name: name, IsNotFound: true}
}

func (s *spyRemote) Addr() netip.Addr { return netip.MustParseAddr("10.9.0.2") }

func (s *spyRemote) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.asked...)
}

// F1 (playground egress threat model, section 2.3): Multi.Resolve asked every
// network, so the operator of a relay saw every lookup, those a local
// container answered and the typos included.
func TestMultiResolveDoesNotLeakToRemote(t *testing.T) {
	ctx := context.Background()
	local, err := NewLocal(netip.MustParsePrefix("10.8.0.0/24"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := local.Attach(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	app, _ := local.Attach(ctx, "app")
	spy := &spyRemote{}
	m := NewMulti()
	if err := m.Add(ctx, "bridge", app); err != nil {
		t.Fatal(err)
	}
	if err := m.Add(ctx, "class", spy); err != nil {
		t.Fatal(err)
	}

	// A name a local network holds goes no further.
	if got, err := m.Resolve(ctx, "web"); err != nil || len(got) != 1 {
		t.Fatalf("local name = %v, %v", got, err)
	}
	// A literal asks nobody.
	if got, err := m.Resolve(ctx, "93.184.216.34"); err != nil || len(got) != 1 {
		t.Fatalf("literal = %v, %v", got, err)
	}
	// A name that is no hostname never leaves.
	for _, bad := range []string{"", "my_secret_db", "a b", "x..y", "-lead", "http://internal/x", string(make([]byte, 300))} {
		if _, err := m.Resolve(ctx, bad); err == nil {
			t.Errorf("%q resolved", bad)
		}
	}
	if n := spy.names(); len(n) != 0 {
		t.Fatalf("the remote network was asked %q", n)
	}

	// A hostname no local network holds is asked of the remote one, which
	// is how a class name resolves.
	if got, err := m.Resolve(ctx, "Mate.Student."); err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("10.9.0.7") {
		t.Fatalf("class name = %v, %v", got, err)
	}
	if n := spy.names(); len(n) != 1 {
		t.Fatalf("remote asked %q, want one name", n)
	}
	// An unknown hostname is a not-found error, as before.
	if _, err := m.Resolve(ctx, "nobody"); err == nil {
		t.Error("an unknown name resolved")
	}
}

func TestRemoteName(t *testing.T) {
	for name, want := range map[string]bool{
		"web": true, "mate.student": true, "A.b-c.example.com.": true, "x": true,
		"": false, ".": false, "a_b": false, "a b": false, "a..b": false,
		"-a": false, "a-": false, "a/b": false, "a:80": false,
	} {
		if got := RemoteName(name); got != want {
			t.Errorf("RemoteName(%q) = %v", name, got)
		}
	}
}

// FuzzRemoteName: whatever is accepted is a plain hostname, so nothing else
// can reach a relay operator.
func FuzzRemoteName(f *testing.F) {
	for _, s := range []string{"web", "a.b.", "my_db", "", "a..b", "x:80", "\x00"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		if !RemoteName(s) {
			return
		}
		if len(s) > 254 {
			t.Fatalf("accepted %d bytes", len(s))
		}
		for i := 0; i < len(s); i++ {
			c := s[i]
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
				t.Fatalf("accepted %q", s)
			}
		}
	})
}

func TestRemoteNameLength(t *testing.T) {
	l63 := strings.Repeat("a", 63)
	if n := l63 + "." + l63 + "." + l63 + "." + strings.Repeat("a", 61); len(n) != 253 || !RemoteName(n) {
		t.Errorf("a 253-byte name: %d bytes, accepted %v", len(n), RemoteName(n))
	}
	if n := l63 + "." + l63 + "." + l63 + "." + strings.Repeat("a", 62); len(n) != 254 || RemoteName(n) {
		t.Errorf("a 254-byte name: %d bytes, accepted %v", len(n), RemoteName(n))
	}
}

// A container on a remote network alone: a literal still asks nobody.
func TestMultiResolveLiteralAsksNobody(t *testing.T) {
	spy := &spyRemote{}
	m := NewMulti()
	if err := m.Add(context.Background(), "class", spy); err != nil {
		t.Fatal(err)
	}
	if got, err := m.Resolve(context.Background(), "203.0.113.9"); err != nil || len(got) != 1 {
		t.Fatalf("literal = %v, %v", got, err)
	}
	if n := spy.names(); len(n) != 0 {
		t.Fatalf("asked %q", n)
	}
}
