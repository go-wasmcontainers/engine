package wasi

import (
	"context"
	"encoding/binary"
	"net/netip"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/internal/testutil"
	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/rootfs"
)

// finish runs a Result's waits as an executor does, bounded.
func finish(t testing.TB, r Result, m Memory) Result {
	for i := 0; r.Wait != nil; i++ {
		var timeout <-chan time.Time
		if r.Sleep > 0 {
			timeout = time.After(r.Sleep)
		}
		select {
		case <-r.Wait:
		case <-timeout:
		case <-time.After(5 * time.Second):
			t.Fatalf("a wait never ended")
		}
		r = r.Resume(m)
	}
	return r
}

func netProc(t testing.TB, n *netdriver.Local, name string) *Proc {
	ep, err := n.Attach(context.Background(), name)
	if err != nil {
		t.Fatal(err)
	}
	p := New(Config{FS: rootfs.New(), Net: netdriver.WithDNS(ep)})
	t.Cleanup(p.Close)
	return p
}

// addr writes a V2 address argument at off: {buf, 128} then the storage.
func addr(m mem, off uint32, a netip.Addr) uint64 {
	binary.LittleEndian.PutUint32(m[off:], off+8)
	binary.LittleEndian.PutUint32(m[off+4:], 128)
	clear(m[off+8 : off+8+128])
	if a.Is4() {
		binary.LittleEndian.PutUint16(m[off+8:], afInet4)
		b := a.As4()
		copy(m[off+10:], b[:])
	} else {
		binary.LittleEndian.PutUint16(m[off+8:], afInet6)
		b := a.As16()
		copy(m[off+10:], b[:])
	}
	return uint64(off)
}

func c(t *testing.T, p *Proc, m mem, name string, args ...uint64) Result {
	t.Helper()
	return finish(t, call(t, p, m, name, args...), m)
}

func le(m mem, off uint32) uint32 { return binary.LittleEndian.Uint32(m[off:]) }

func TestSocketStream(t *testing.T) {
	n, _ := netdriver.NewLocal(netdriver.DefaultPrefix)
	defer n.Close()
	srv, cli := netProc(t, n, "srv"), netProc(t, n, "cli")
	ms, mc := make(mem, 4096), make(mem, 4096)

	// Server: open, bind 0.0.0.0:80, listen, non-blocking accept.
	if r := c(t, srv, ms, "sock_open", afInet4, sockStream, 0); r.Errno != 0 {
		t.Fatal(r)
	}
	lfd := uint64(le(ms, 0))
	if r := c(t, srv, ms, "fd_fdstat_set_flags", lfd, fdflagNonblock); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := c(t, srv, ms, "sock_bind", lfd, addr(ms, 100, netip.IPv4Unspecified()), 80); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := c(t, srv, ms, "sock_bind", lfd, addr(ms, 100, netip.MustParseAddr("10.9.9.9")), 81); r.Errno != EINVAL {
		t.Fatal("second bind:", r)
	}
	if r := c(t, srv, ms, "sock_listen", lfd, 16); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := call(t, srv, ms, "sock_accept", lfd, 0, 8); r.Errno != EAGAIN {
		t.Fatal("accept with nothing pending:", r)
	}
	if r := c(t, srv, ms, "fd_fdstat_get", lfd, 300); r.Errno != 0 || ms[300] != ftSocketStream || binary.LittleEndian.Uint16(ms[302:]) != fdflagNonblock {
		t.Fatalf("fdstat %v %v", r, ms[300:304])
	}
	if r := c(t, srv, ms, "sock_getlocaladdr", lfd, addr(ms, 400, netip.IPv4Unspecified()), 600); r.Errno != 0 || le(ms, 600) != 80 ||
		netip.AddrFrom4([4]byte(ms[410:414])) != srv.cfg.Net.Addr() {
		t.Fatalf("local addr %v port %d", ms[408:414], le(ms, 600))
	}

	// Client: a blocking connect (Wait, then Resume).
	c(t, cli, mc, "sock_open", afInet4, sockStream, 0)
	cfd := uint64(le(mc, 0))
	if r := c(t, cli, mc, "sock_connect", cfd, addr(mc, 100, srv.cfg.Net.Addr()), 80); r.Errno != 0 {
		t.Fatal("connect:", r)
	}
	if r := c(t, cli, mc, "sock_connect", cfd, addr(mc, 100, srv.cfg.Net.Addr()), 80); r.Errno != EISCONN {
		t.Fatal("connect twice:", r)
	}

	// poll_oneoff on the listener wakes when the connection arrives.
	sub := make([]byte, 48)
	binary.LittleEndian.PutUint64(sub, 42)
	sub[8] = eventFdRead
	binary.LittleEndian.PutUint32(sub[16:], uint32(lfd))
	copy(ms[1000:], sub)
	if r := c(t, srv, ms, "poll_oneoff", 1000, 1100, 1, 1200); r.Errno != 0 || le(ms, 1200) != 1 || binary.LittleEndian.Uint64(ms[1100:]) != 42 {
		t.Fatalf("poll: %v n=%d", r, le(ms, 1200))
	}
	if r := c(t, srv, ms, "sock_accept", lfd, 0, 8); r.Errno != 0 {
		t.Fatal(r)
	}
	afd := uint64(le(ms, 8))

	// Client sends through fd_write, server reads with sock_recv.
	msg := []byte("hello over the class network")
	copy(mc[2000:], msg)
	binary.LittleEndian.PutUint32(mc[1900:], 2000)
	binary.LittleEndian.PutUint32(mc[1904:], uint32(len(msg)))
	if r := c(t, cli, mc, "fd_write", cfd, 1900, 1, 1950); r.Errno != 0 || le(mc, 1950) != uint32(len(msg)) {
		t.Fatal(r)
	}
	binary.LittleEndian.PutUint32(ms[1900:], 2000)
	binary.LittleEndian.PutUint32(ms[1904:], 100)
	if r := c(t, srv, ms, "sock_recv", afd, 1900, 1, 0, 1950, 1960); r.Errno != 0 || string(ms[2000:2000+le(ms, 1950)]) != string(msg) {
		t.Fatalf("recv %v %q", r, ms[2000:2000+le(ms, 1950)])
	}
	// The peer address is the client's.
	if r := c(t, srv, ms, "sock_getpeeraddr", afd, addr(ms, 400, netip.IPv4Unspecified()), 600); r.Errno != 0 ||
		netip.AddrFrom4([4]byte(ms[410:414])) != cli.cfg.Net.Addr() {
		t.Fatalf("peer %v", ms[408:414])
	}
	// Half-close: the server reads EOF (0 bytes), then shutdown again is
	// fine; a write after shutdown(WR) is EPIPE.
	if r := c(t, cli, mc, "sock_shutdown", cfd, shutWr); r.Errno != 0 {
		t.Fatal(r)
	}
	if r := c(t, srv, ms, "fd_read", afd, 1900, 1, 1950); r.Errno != 0 || le(ms, 1950) != 0 {
		t.Fatalf("EOF: %v %d", r, le(ms, 1950))
	}
	if r := c(t, cli, mc, "fd_write", cfd, 1900, 1, 1950); r.Errno != EPIPE {
		t.Fatal("write after shutdown:", r)
	}
	// Refused, unreachable.
	c(t, cli, mc, "sock_open", afInet4, sockStream, 0)
	fd2 := uint64(le(mc, 0))
	if r := c(t, cli, mc, "sock_connect", fd2, addr(mc, 100, srv.cfg.Net.Addr()), 9); r.Errno != ECONNREFUSED {
		t.Fatal("refused:", r)
	}
	c(t, cli, mc, "sock_open", afInet4, sockStream, 0)
	fd3 := uint64(le(mc, 0))
	c(t, cli, mc, "fd_fdstat_set_flags", fd3, fdflagNonblock)
	if r := call(t, cli, mc, "sock_connect", fd3, addr(mc, 100, netip.MustParseAddr("10.88.200.1")), 80); r.Errno != EINPROGRESS {
		t.Fatal("non-blocking connect:", r)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		c(t, cli, mc, "sock_getsockopt", fd3, 0, soError, 700, 4)
		if v := le(mc, 700); v != 0 {
			if v != EHOSTUNREACH {
				t.Fatalf("SO_ERROR %d", v)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no SO_ERROR")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Closing releases the listener: a new connect is refused.
	if r := c(t, srv, ms, "fd_close", lfd); r.Errno != 0 {
		t.Fatal(r)
	}
	c(t, cli, mc, "sock_open", afInet4, sockStream, 0)
	fd4 := uint64(le(mc, 0))
	if r := c(t, cli, mc, "sock_connect", fd4, addr(mc, 100, srv.cfg.Net.Addr()), 80); r.Errno != ECONNREFUSED {
		t.Fatal("after close:", r)
	}
}

func TestSocketDatagram(t *testing.T) {
	n, _ := netdriver.NewLocal(netdriver.DefaultPrefix)
	defer n.Close()
	a, b := netProc(t, n, "a"), netProc(t, n, "b")
	ma, mb := make(mem, 4096), make(mem, 4096)
	c(t, b, mb, "sock_open", afInet4, sockDgram, 0)
	bfd := uint64(le(mb, 0))
	if r := c(t, b, mb, "sock_bind", bfd, addr(mb, 100, netip.IPv4Unspecified()), 53); r.Errno != 0 {
		t.Fatal(r)
	}
	c(t, a, ma, "sock_open", afInet4, sockDgram, 0)
	afd := uint64(le(ma, 0))
	payload := []byte("0123456789")
	copy(ma[2000:], payload)
	binary.LittleEndian.PutUint32(ma[1900:], 2000)
	binary.LittleEndian.PutUint32(ma[1904:], uint32(len(payload)))
	// An unbound socket's send_to binds it to an ephemeral port.
	if r := c(t, a, ma, "sock_send_to", afd, 1900, 1, addr(ma, 100, b.cfg.Net.Addr()), 53, 0, 1950); r.Errno != 0 || le(ma, 1950) != 10 {
		t.Fatal(r)
	}
	// A 4-byte buffer: truncated, with the flag, and the source.
	binary.LittleEndian.PutUint32(mb[1900:], 2000)
	binary.LittleEndian.PutUint32(mb[1904:], 4)
	r := c(t, b, mb, "sock_recv_from", bfd, 1900, 1, addr(mb, 400, netip.IPv4Unspecified()), 0, 600, 1950, 1960)
	if r.Errno != 0 || le(mb, 1950) != 4 || string(mb[2000:2004]) != "0123" || mb[1960] != roDataTruncated {
		t.Fatalf("recv_from %v n=%d %q flags=%d", r, le(mb, 1950), mb[2000:2004], mb[1960])
	}
	if from := netip.AddrFrom4([4]byte(mb[410:414])); from != a.cfg.Net.Addr() || le(mb, 600) < 49152 {
		t.Fatalf("from %s:%d", from, le(mb, 600))
	}
	// Oversized: EMSGSIZE.
	binary.LittleEndian.PutUint32(ma[1904:], netdriver.MaxDatagram+1)
	if r := c(t, a, ma, "sock_send_to", afd, 1900, 1, addr(ma, 100, b.cfg.Net.Addr()), 53, 0, 1950); r.Errno != EMSGSIZE && r.Errno != EFAULT {
		t.Fatal("oversized:", r)
	}
}

func TestGetAddrInfo(t *testing.T) {
	n, _ := netdriver.NewLocal(netdriver.DefaultPrefix)
	defer n.Close()
	web := netProc(t, n, "web")
	p := netProc(t, n, "cli")
	m := make(mem, 4096)
	// Two result slots chained, each with a sockaddr and 26 bytes of data,
	// as stealthrocket/net allocates them.
	const res0, res1, sa0, sa1, d0, d1 = 1000, 1100, 1200, 1300, 1400, 1500
	for _, s := range [][3]uint32{{res0, sa0, d0}, {res1, sa1, d1}} {
		binary.LittleEndian.PutUint32(m[s[0]+12:], s[1])
		binary.LittleEndian.PutUint32(m[s[1]+4:], 26)
		binary.LittleEndian.PutUint32(m[s[1]+8:], s[2])
	}
	binary.LittleEndian.PutUint32(m[res0+24:], res1)
	binary.LittleEndian.PutUint32(m[900:], res0) // *res
	node, nl := put(m, 100, "web\x00")
	serv, sl := put(m, 200, "8080\x00")
	m[302] = afUnspec
	m[303] = sockStream
	if r := c(t, p, m, "sock_getaddrinfo", node, nl, serv, sl, 300, 900, 2, 950); r.Errno != 0 || le(m, 950) != 1 {
		t.Fatalf("%v count %d", r, le(m, 950))
	}
	if m[sa0] != afInet4 || binary.BigEndian.Uint16(m[d0:]) != 8080 || netip.AddrFrom4([4]byte(m[d0+2:d0+6])) != web.cfg.Net.Addr() {
		t.Fatalf("result %v %v", m[sa0], m[d0:d0+6])
	}
	node, nl = put(m, 100, "nobody\x00")
	if r := c(t, p, m, "sock_getaddrinfo", node, nl, serv, sl, 300, 900, 2, 950); r.Errno != EAI_NONAME {
		t.Fatal("nobody:", r)
	}
	serv, sl = put(m, 200, "gopher\x00")
	if r := c(t, p, m, "sock_getaddrinfo", node, nl, serv, sl, 300, 900, 2, 950); r.Errno != EAI_SERVICE {
		t.Fatal("unknown service:", r)
	}
}

// FuzzSockets runs sequences of socket calls with fuzzed arguments on
// two processes of one network: no panic, no wait that never ends.
func FuzzSockets(f *testing.F) {
	for _, s := range socketSeeds() {
		f.Add(s)
	}
	names := []string{"sock_open", "sock_bind", "sock_listen", "sock_connect", "sock_accept", "sock_recv", "sock_send",
		"sock_recv_from", "sock_send_to", "sock_getsockopt", "sock_setsockopt", "sock_getlocaladdr", "sock_getpeeraddr",
		"sock_getaddrinfo", "sock_shutdown", "fd_read", "fd_write", "fd_close", "fd_fdstat_set_flags", "poll_oneoff", "fd_fdstat_get"}
	f.Fuzz(func(t *testing.T, ops []byte) {
		n, _ := netdriver.NewLocal(netdriver.DefaultPrefix)
		defer n.Close()
		procs := []*Proc{netProc(t, n, "x"), netProc(t, n, "y")}
		ms := []mem{make(mem, 1024), make(mem, 1024)}
		// Plant valid-looking addresses so calls get past the parsers.
		for i := range ms {
			addr(ms[i], 512, procs[1-i].cfg.Net.Addr())
		}
		for len(ops) >= 4 && len(ops) < 4096 {
			pi := int(ops[0]) & 1
			name := names[int(ops[1])%len(names)]
			fn := FuncIndex[name]
			args := make([]uint64, len(Funcs[fn].Params))
			for j := range args {
				v := uint64(ops[2]) ^ uint64(j)*uint64(ops[3])
				switch {
				case j == 0:
					v = 3 + v%4 // a likely descriptor
				case ops[3]&1 == 1:
					v = 512 // the planted address
				default:
					v %= 1100
				}
				args[j] = v
			}
			r := procs[pi].Call(fn, args, ms[pi])
			// Waits are bounded: a blocked call is abandoned, as a kill would.
			for k := 0; r.Wait != nil && k < 3; k++ {
				select {
				case <-r.Wait:
				case <-time.After(5 * time.Millisecond):
				}
				r = r.Resume(ms[pi])
			}
			ops = ops[4:]
		}
	})
}

func socketSeeds() [][]byte {
	return [][]byte{
		{0, 0, 1, 2, 0, 1, 0, 1, 0, 2, 0, 0, 1, 0, 1, 2, 1, 3, 0, 1},
		{0, 0, 2, 1, 0, 1, 0, 1, 1, 8, 1, 1, 0, 7, 0, 0},
		{0, 13, 3, 3, 1, 19, 0, 0, 0, 20, 3, 3},
	}
}

func TestWriteSocketCorpus(t *testing.T) {
	seeds := socketSeeds()
	vals := make([]any, len(seeds))
	for i, s := range seeds {
		vals[i] = testutil.Tuple{s}
	}
	testutil.WriteCorpus(t, "FuzzSockets", vals...)
}
