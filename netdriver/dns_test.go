package netdriver

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/go-wasmcontainers/engine/internal/testutil"
)

func query(t testing.TB, name string, typ dnsmessage.Type) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: 0x1234, RecursionDesired: true})
	b.StartQuestions()
	b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET})
	out, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func parse(t *testing.T, b []byte) (dnsmessage.Header, []dnsmessage.Resource) {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		t.Fatalf("answer does not parse: %v", err)
	}
	return m.Header, m.Answers
}

// TestDNSThroughGoResolver asks the network's DNS with Go's own resolver,
// as a guest does: the stub dials 127.0.0.1:53 over UDP.
func TestDNSThroughGoResolver(t *testing.T) {
	n := newNet(t)
	web := attach(t, n, "web")
	client := WithDNS(attach(t, n, "client"))
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
		if network != "udp" {
			t.Errorf("resolver dialed %s", network)
		}
		return client.Dial(ctx, "udp", netip.MustParseAddrPort("127.0.0.1:53"))
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ips, err := r.LookupNetIP(ctx, "ip4", "web.")
	if err != nil || len(ips) != 1 || ips[0] != web.Addr() {
		t.Fatalf("web: %v %v", ips, err)
	}
	_, err = r.LookupNetIP(ctx, "ip", "nobody.")
	if d := (*net.DNSError)(nil); !asDNS(err, &d) || !d.IsNotFound {
		t.Fatalf("nobody: %v", err)
	}
}

func asDNS(err error, d **net.DNSError) bool {
	e, ok := err.(*net.DNSError)
	*d = e
	return ok
}

func TestAnswer(t *testing.T) {
	resolve := func(_ context.Context, name string) ([]netip.Addr, error) {
		switch name {
		case "web":
			return []netip.Addr{netip.MustParseAddr("10.88.0.2")}, nil
		case "many":
			var out []netip.Addr
			for i := 0; i < 100; i++ {
				out = append(out, netip.AddrFrom4([4]byte{10, 0, 0, byte(i)}))
			}
			return out, nil
		case "broken":
			return nil, context.DeadlineExceeded
		}
		return nil, &net.DNSError{Name: name, IsNotFound: true}
	}
	h, ans := parse(t, Answer(ctx, resolve, query(t, "web.", dnsmessage.TypeA)))
	if h.ID != 0x1234 || !h.Response || h.RCode != dnsmessage.RCodeSuccess || len(ans) != 1 {
		t.Fatalf("web A: %+v %v", h, ans)
	}
	if a := ans[0].Body.(*dnsmessage.AResource).A; netip.AddrFrom4(a).String() != "10.88.0.2" || ans[0].Header.TTL != DNSTTL {
		t.Errorf("web A = %v", ans[0])
	}
	if h, ans := parse(t, Answer(ctx, resolve, query(t, "WEB.", dnsmessage.TypeAAAA))); h.RCode != dnsmessage.RCodeSuccess || len(ans) != 0 {
		t.Errorf("web AAAA: %+v %v (want NOERROR, no record)", h, ans)
	}
	if h, _ := parse(t, Answer(ctx, resolve, query(t, "nobody.", dnsmessage.TypeA))); h.RCode != dnsmessage.RCodeNameError {
		t.Errorf("nobody: %v", h.RCode)
	}
	if h, _ := parse(t, Answer(ctx, resolve, query(t, "broken.", dnsmessage.TypeA))); h.RCode != dnsmessage.RCodeServerFailure {
		t.Errorf("broken: %v", h.RCode)
	}
	big := Answer(ctx, resolve, query(t, "many.", dnsmessage.TypeA))
	if h, ans := parse(t, big); len(big) > MaxDNSMessage || !h.Truncated || len(ans) == 0 {
		t.Errorf("many: %d bytes, TC=%v, %d answers", len(big), h.Truncated, len(ans))
	}
	// Not a query: no answer.
	resp := query(t, "web.", dnsmessage.TypeA)
	resp[2] |= 0x80
	if Answer(ctx, resolve, resp) != nil || Answer(ctx, resolve, []byte{1, 2}) != nil {
		t.Error("answered a response or a runt")
	}
	// Garbage after a header: FORMERR with the query's ID.
	junk := append([]byte{0xab, 0xcd, 0, 0, 0, 5, 0, 0, 0, 0, 0, 0}, 0xff)
	if h, _ := parse(t, Answer(ctx, resolve, junk)); h.ID != 0xabcd || h.RCode != dnsmessage.RCodeFormatError {
		t.Errorf("junk: %+v", h)
	}
}

func TestWriteDNSCorpus(t *testing.T) {
	testutil.WriteCorpus(t, "FuzzAnswer",
		query(t, "web.", dnsmessage.TypeA), query(t, "nobody.", dnsmessage.TypeAAAA),
		[]byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 3, 'w', 'e', 'b', 0, 0, 1, 0, 1})
}

// FuzzAnswer: whatever arrives, the answer is nil or a response that
// parses, carries the query's ID and fits in a UDP datagram.
func FuzzAnswer(f *testing.F) {
	f.Add(query(f, "web.", dnsmessage.TypeA))
	f.Add(query(f, "nobody.", dnsmessage.TypeAAAA))
	f.Add([]byte{0, 1, 0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 3, 'w', 'e', 'b', 0, 0, 1, 0, 1})
	resolve := func(_ context.Context, name string) ([]netip.Addr, error) {
		if len(name)%2 == 0 {
			return []netip.Addr{netip.MustParseAddr("10.88.0.2"), netip.MustParseAddr("fd00::2")}, nil
		}
		return nil, &net.DNSError{Name: name, IsNotFound: true}
	}
	f.Fuzz(func(t *testing.T, q []byte) {
		a := Answer(context.Background(), resolve, q)
		if a == nil {
			return
		}
		if len(a) > MaxDNSMessage {
			t.Fatalf("%d-byte answer", len(a))
		}
		var m dnsmessage.Message
		if err := m.Unpack(a); err != nil {
			t.Fatalf("answer does not parse: %v", err)
		}
		if !m.Response || len(q) >= 2 && m.ID != uint16(q[0])<<8|uint16(q[1]) {
			t.Fatalf("header %+v", m.Header)
		}
	})
}
