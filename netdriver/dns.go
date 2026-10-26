package netdriver

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Resolvers are the addresses where a container's stub resolver finds
// the network's DNS: 127.0.0.11 (Docker's embedded DNS) and 127.0.0.1 /
// ::1, which Go's and musl's resolvers use when /etc/resolv.conf is
// missing — as it is in most wasm images.
var Resolvers = []netip.Addr{
	netip.AddrFrom4([4]byte{127, 0, 0, 11}),
	netip.AddrFrom4([4]byte{127, 0, 0, 1}),
	netip.IPv6Loopback(),
}

// MaxDNSMessage bounds a query and an answer: plain DNS over UDP, no EDNS.
const MaxDNSMessage = 512

// DNSTTL is the time to live of every answer, in seconds: addresses are
// stable while a container is attached, but containers come and go.
const DNSTTL = 5

// WithDNS returns ep with a DNS server on its resolver addresses: a UDP
// socket connected to one of Resolvers, port 53, is answered in the
// engine from ep.Resolve — A and AAAA records for the network's names,
// NXDOMAIN for anything else. Nothing is forwarded outside the network.
// So a stock Go or C program resolves container names with no
// sock_getaddrinfo. (A container's own UDP server on those addresses,
// port 53, is shadowed.)
func WithDNS(ep Endpoint) Endpoint { return &dnsEP{Endpoint: ep} }

type dnsEP struct{ Endpoint }

func isResolver(ap netip.AddrPort) bool {
	if ap.Port() != 53 {
		return false
	}
	for _, r := range Resolvers {
		if ap.Addr().Unmap() == r {
			return true
		}
	}
	return false
}

func (e *dnsEP) Dial(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error) {
	if network == "udp" && isResolver(addr) {
		return newDNSConn(e.Endpoint, addr), nil
	}
	return e.Endpoint.Dial(ctx, network, addr)
}

// dnsConn is a connected UDP socket whose peer is the network's DNS.
type dnsConn struct {
	ep    Endpoint
	local netip.AddrPort
	peer  netip.AddrPort

	mu       sync.Mutex
	answers  [][]byte
	closed   bool
	deadline time.Time
	notify   chan struct{}
	ctx      context.Context
	cancel   context.CancelFunc
}

func newDNSConn(ep Endpoint, peer netip.AddrPort) *dnsConn {
	ctx, cancel := context.WithCancel(context.Background())
	local := netip.AddrPortFrom(peer.Addr(), 40000) // a loopback socket of the container
	return &dnsConn{ep: ep, local: local, peer: peer, notify: make(chan struct{}), ctx: ctx, cancel: cancel}
}

func (c *dnsConn) changed() {
	close(c.notify)
	c.notify = make(chan struct{})
}

// maxPendingAnswers bounds the answers a guest has not read yet.
const maxPendingAnswers = 16

func (c *dnsConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return 0, net.ErrClosed
	}
	if len(b) > MaxDNSMessage {
		return len(b), nil // dropped, as an oversized datagram would be
	}
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	ans := Answer(ctx, c.ep.Resolve, b)
	if ans == nil {
		return len(b), nil // not a query: no answer, as a server ignores it
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed && len(c.answers) < maxPendingAnswers {
		c.answers = append(c.answers, ans)
		c.changed()
	}
	return len(b), nil
}

func (c *dnsConn) Read(b []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		switch {
		case c.closed:
			return 0, net.ErrClosed
		case past(c.deadline):
			return 0, &net.OpError{Op: "read", Net: "udp", Addr: c.RemoteAddr(), Err: os.ErrDeadlineExceeded}
		case len(c.answers) > 0:
			a := c.answers[0]
			c.answers = c.answers[1:]
			return copy(b, a), nil
		}
		ch, dl := c.notify, c.deadline
		c.mu.Unlock()
		err := waitUntil(ch, dl)
		c.mu.Lock()
		if err != nil {
			return 0, &net.OpError{Op: "read", Net: "udp", Addr: c.RemoteAddr(), Err: err}
		}
	}
}

func (c *dnsConn) ReadFrom(b []byte) (int, net.Addr, error) {
	k, err := c.Read(b)
	return k, c.RemoteAddr(), err
}

func (c *dnsConn) WriteTo(b []byte, addr net.Addr) (int, error) {
	if ua, ok := addr.(*net.UDPAddr); ok && isResolver(ua.AddrPort()) {
		return c.Write(b)
	}
	return len(b), nil // a connected DNS socket sends nowhere else
}

func (c *dnsConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.closed {
		c.closed = true
		c.cancel()
		c.answers = nil
		c.changed()
	}
	return nil
}

func (c *dnsConn) LocalAddr() net.Addr  { return net.UDPAddrFromAddrPort(c.local) }
func (c *dnsConn) RemoteAddr() net.Addr { return net.UDPAddrFromAddrPort(c.peer) }

func (c *dnsConn) SetDeadline(t time.Time) error { return c.SetReadDeadline(t) }

func (c *dnsConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.deadline = t
	c.changed()
	c.mu.Unlock()
	return nil
}

func (c *dnsConn) SetWriteDeadline(time.Time) error { return nil }

// Answer answers one DNS query (RFC 1035) from resolve: A and AAAA records
// of a name that resolves, NOERROR with no record for its other types,
// NXDOMAIN for a name that does not, SERVFAIL when resolve fails
// otherwise, FORMERR for a malformed query, NOTIMP for an opcode other
// than QUERY. It returns nil for what is not a query (a response, or too
// short to carry an ID). Answers fit in MaxDNSMessage bytes, truncated
// (TC) otherwise.
func Answer(ctx context.Context, resolve func(context.Context, string) ([]netip.Addr, error), query []byte) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(query)
	if err != nil {
		if len(query) < 12 || query[2]&0x80 != 0 {
			return nil
		}
		// A header that does not parse past its ID: FORMERR.
		return reply(dnsmessage.Header{ID: uint16(query[0])<<8 | uint16(query[1]), RCode: dnsmessage.RCodeFormatError}, nil, nil)
	}
	if h.Response {
		return nil
	}
	rh := dnsmessage.Header{ID: h.ID, Response: true, OpCode: h.OpCode, RecursionDesired: h.RecursionDesired, Authoritative: true}
	if h.OpCode != 0 {
		rh.RCode = dnsmessage.RCodeNotImplemented
		return reply(rh, nil, nil)
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) != 1 {
		rh.RCode = dnsmessage.RCodeFormatError
		return reply(rh, nil, nil)
	}
	q := qs[0]
	if q.Class != dnsmessage.ClassINET && q.Class != dnsmessage.ClassANY {
		rh.RCode = dnsmessage.RCodeRefused
		return reply(rh, &q, nil)
	}
	name := strings.ToLower(strings.TrimSuffix(q.Name.String(), "."))
	addrs, err := resolve(ctx, name)
	if err != nil {
		var d *net.DNSError
		if errors.As(err, &d) && d.IsNotFound {
			rh.RCode = dnsmessage.RCodeNameError
		} else {
			rh.RCode = dnsmessage.RCodeServerFailure
		}
		return reply(rh, &q, nil)
	}
	var ans []dnsmessage.Resource
	for _, a := range addrs {
		a = a.Unmap()
		rr := dnsmessage.ResourceHeader{Name: q.Name, Class: dnsmessage.ClassINET, TTL: DNSTTL}
		switch {
		case a.Is4() && (q.Type == dnsmessage.TypeA || q.Type == dnsmessage.TypeALL):
			rr.Type = dnsmessage.TypeA
			ans = append(ans, dnsmessage.Resource{Header: rr, Body: &dnsmessage.AResource{A: a.As4()}})
		case a.Is6() && (q.Type == dnsmessage.TypeAAAA || q.Type == dnsmessage.TypeALL):
			rr.Type = dnsmessage.TypeAAAA
			ans = append(ans, dnsmessage.Resource{Header: rr, Body: &dnsmessage.AAAAResource{AAAA: a.As16()}})
		}
	}
	return reply(rh, &q, ans)
}

// reply builds a response, dropping answers (and setting TC) past
// MaxDNSMessage.
func reply(h dnsmessage.Header, q *dnsmessage.Question, ans []dnsmessage.Resource) []byte {
	for {
		b := dnsmessage.NewBuilder(make([]byte, 0, MaxDNSMessage), h)
		b.EnableCompression()
		if err := b.StartQuestions(); err != nil {
			return nil
		}
		if q != nil {
			if err := b.Question(*q); err != nil {
				return nil
			}
		}
		if err := b.StartAnswers(); err != nil {
			return nil
		}
		for _, r := range ans {
			var err error
			switch body := r.Body.(type) {
			case *dnsmessage.AResource:
				err = b.AResource(r.Header, *body)
			case *dnsmessage.AAAAResource:
				err = b.AAAAResource(r.Header, *body)
			}
			if err != nil {
				return nil
			}
		}
		out, err := b.Finish()
		if err != nil {
			return nil
		}
		if len(out) <= MaxDNSMessage {
			return out
		}
		h.Truncated = true
		ans = ans[:len(ans)/2]
	}
}
