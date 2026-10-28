package wasi

// Sockets: the WasmEdge socket extension of wasi_snapshot_preview1 (V2,
// as github.com/stealthrocket/wasi-go serves it and
// github.com/stealthrocket/net calls it), plus preview 1's own
// sock_accept, sock_recv, sock_send and sock_shutdown, over a
// netdriver.Endpoint. References: docs/REFERENCES.md.
//
// The host never blocks inside Call: every operation that waits (on the
// network or on the guest's peer) runs in a goroutine, and a blocking call
// returns a Result with Wait and Resume, which the executor honours. The
// guest's own non-blocking mode (fd_fdstat_set_flags NONBLOCK, which Go's
// net package sets) gets EAGAIN / EINPROGRESS instead, and poll_oneoff
// waits on the sockets' readiness.

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// Socket constants (WasmEdge's thirdparty/wasi/api.hpp; preview 1 for the
// file types and flags).
const (
	afUnspec = 0
	afInet4  = 1
	afInet6  = 2
	afUnix   = 3

	sockAny    = 0
	sockDgram  = 1
	sockStream = 2

	ftSocketDgram  = 5
	ftSocketStream = 6

	fdflagNonblock = 4

	soReuseAddr  = 0
	soType       = 1
	soError      = 2
	soBroadcast  = 4
	soSndBuf     = 5
	soRcvBuf     = 6
	soKeepAlive  = 7
	soAcceptConn = 13

	riPeek          = 1
	roDataTruncated = 1

	shutRd = 1
	shutWr = 2

	aiPassive     = 1
	aiNumericHost = 4

	ipprotoTCP = 1
	ipprotoUDP = 2

	eventHangup = 1
)

// More errno values of wasi_snapshot_preview1 (enum order), and the
// getaddrinfo ones WasmEdge adds.
const (
	EADDRINUSE    = 3
	EADDRNOTAVAIL = 4
	EAFNOSUPPORT  = 5
	EALREADY      = 7
	ECONNABORTED  = 13
	ECONNREFUSED  = 14
	ECONNRESET    = 15
	EHOSTUNREACH  = 23
	EINPROGRESS   = 26
	EINTR         = 27
	EISCONN       = 30
	EMFILE        = 33
	EMSGSIZE      = 35
	ENETDOWN      = 38
	ENETUNREACH   = 40
	ENOBUFS       = 42
	ENOPROTOOPT   = 50
	ENOTCONN      = 53
	ENOTSOCK      = 57
	EPIPE         = 64
	EPROTOTYPE    = 67
	ETIMEDOUT     = 73

	EAI_NONAME  = 84
	EAI_SERVICE = 85
)

// sockBuf bounds what one socket holds in the host, each way.
const sockBuf = 64 << 10

type sock struct {
	stype    uint8
	family   uint8
	nonblock bool
	port     uint16 // bound before listen
	bound    bool
	reuse    bool

	listener   net.Listener
	conn       net.Conn
	pc         net.PacketConn
	local      netip.AddrPort
	peer       netip.AddrPort
	connecting bool
	soErr      error // a finished non-blocking connect's error, for SO_ERROR

	// Filled by the socket's goroutines, under Proc.nmu.
	rbuf     []byte
	rEOF     bool
	rErr     error
	wbuf     []byte
	wErr     error
	wShut    bool // shutdown(WR) asked: close the write side once drained
	rShut    bool
	accepted []net.Conn
	aErr     error
	dgrams   []dgram
	dq       []dgram // datagrams to send
	closed   bool
}

type dgram struct {
	addr netip.AddrPort
	b    []byte
}

// netState is the process's network: its endpoint and the bookkeeping the
// socket goroutines share with Call.
type netState struct {
	ep     netdriver.Endpoint
	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex // guards every sock's goroutine-filled fields
	wake   chan struct{}
}

func (n *netState) changed() {
	close(n.wake)
	n.wake = make(chan struct{})
}

// current is the channel the next change closes.
func (n *netState) current() <-chan struct{} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.wake
}

// notify is changed() taking the lock.
func (n *netState) notify() {
	n.mu.Lock()
	n.changed()
	n.mu.Unlock()
}

func (p *Proc) netInit() {
	ctx, cancel := context.WithCancel(context.Background())
	p.net = &netState{ep: p.cfg.Net, ctx: ctx, cancel: cancel, wake: make(chan struct{})}
}

// Close releases what the process holds beyond its memory: its host
// modules' instances, its sockets and their goroutines. An executor calls
// it when the process ends.
func (p *Proc) Close() {
	p.host.closeAll()
	n := p.net
	n.cancel()
	n.mu.Lock()
	var socks []*sock
	for _, f := range p.fds {
		if f.sock != nil {
			socks = append(socks, f.sock)
		}
	}
	n.mu.Unlock()
	for _, s := range socks {
		p.closeSock(s)
	}
}

func (p *Proc) closeSock(s *sock) {
	n := p.net
	n.mu.Lock()
	if s.closed {
		n.mu.Unlock()
		return
	}
	s.closed = true
	for _, c := range s.accepted {
		c.Close()
	}
	s.accepted = nil
	n.changed()
	l, c, pc := s.listener, s.conn, s.pc
	n.mu.Unlock()
	if l != nil {
		l.Close()
	}
	if c != nil {
		c.Close()
	}
	if pc != nil {
		pc.Close()
	}
}

// sockErrno maps a network error to an errno.
func sockErrno(err error) uint32 {
	if err == nil {
		return ESUCCESS
	}
	for _, m := range []struct {
		e syscall.Errno
		w uint32
	}{
		{syscall.ECONNREFUSED, ECONNREFUSED}, {syscall.EHOSTUNREACH, EHOSTUNREACH},
		{syscall.ECONNRESET, ECONNRESET}, {syscall.EADDRINUSE, EADDRINUSE},
		{syscall.ENOBUFS, ENOBUFS}, {syscall.EPIPE, EPIPE}, {syscall.ENETDOWN, ENETDOWN},
		{syscall.ENETUNREACH, ENETUNREACH}, {syscall.EMSGSIZE, EMSGSIZE},
		{syscall.EPROTOTYPE, EPROTOTYPE}, {syscall.ETIMEDOUT, ETIMEDOUT},
		{syscall.ECONNABORTED, ECONNABORTED}, {syscall.EADDRNOTAVAIL, EADDRNOTAVAIL},
		{syscall.EAFNOSUPPORT, EAFNOSUPPORT}, {syscall.EINVAL, EINVAL},
	} {
		if errors.Is(err, m.e) {
			return m.w
		}
	}
	switch {
	case errors.Is(err, os.ErrDeadlineExceeded), errors.Is(err, context.DeadlineExceeded):
		return ETIMEDOUT
	case errors.Is(err, net.ErrClosed), errors.Is(err, context.Canceled):
		return ECONNABORTED
	}
	return EIO
}

// async runs fn in a goroutine and returns a Result that waits for it and
// then answers with then (called in the host's thread, with the memory).
func (p *Proc) async(fn func(), then func(m Memory) Result) Result {
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	return Result{Wait: done, Resume: then}
}

// block waits for any socket change and retries call, unless the socket
// is non-blocking.
//
// wake must have been taken (current) before the socket's state was read,
// so that a change in between is not missed.
func (p *Proc) block(s *sock, wake <-chan struct{}, call func(m Memory) Result) Result {
	if s.nonblock {
		return fail(EAGAIN)
	}
	return Result{Wait: wake, Resume: call}
}

// sockView is what Call reads of a socket's goroutine-set state, taken
// under the lock.
type sockView struct {
	conn       net.Conn
	pc         net.PacketConn
	listener   net.Listener
	connecting bool
	peer       netip.AddrPort
}

func (p *Proc) view(s *sock) sockView {
	p.net.mu.Lock()
	defer p.net.mu.Unlock()
	return sockView{s.conn, s.pc, s.listener, s.connecting, s.peer}
}

func (p *Proc) sockOf(fd uint32) (*sock, uint32) {
	f := p.fds[fd]
	if f == nil {
		return nil, EBADF
	}
	if f.sock == nil {
		return nil, ENOTSOCK
	}
	return f.sock, 0
}

// readAddr reads a WasmEdge address argument: a __wasi_address_t {buf,
// buf_len} whose buffer is 4 or 16 address bytes (V1), or 128 bytes of
// storage, a family (u16) then the address (V2).
func readAddr(m Memory, ptr uint32) (netip.Addr, uint32) {
	h, ok := m.Read(ptr, 8)
	if !ok {
		return netip.Addr{}, EFAULT
	}
	buf, n := le32(h), le32(h[4:])
	switch n {
	case 4, 16, 128:
	default:
		return netip.Addr{}, EINVAL
	}
	b, ok := m.Read(buf, n)
	if !ok {
		return netip.Addr{}, EFAULT
	}
	switch n {
	case 4:
		return netip.AddrFrom4([4]byte(b)), 0
	case 16:
		return netip.AddrFrom16([16]byte(b)).Unmap(), 0
	}
	switch binary.LittleEndian.Uint16(b) {
	case afInet4:
		return netip.AddrFrom4([4]byte(b[2:6])), 0
	case afInet6:
		return netip.AddrFrom16([16]byte(b[2:18])).Unmap(), 0
	}
	return netip.Addr{}, EAFNOSUPPORT
}

// writeAddr fills a WasmEdge address argument with a: 128 bytes of
// storage (family, address), or a bare 4- or 16-byte buffer.
func writeAddr(m Memory, ptr uint32, a netip.Addr) uint32 {
	h, ok := m.Read(ptr, 8)
	if !ok {
		return EFAULT
	}
	buf, n := le32(h), le32(h[4:])
	if !a.IsValid() {
		a = netip.IPv4Unspecified()
	}
	var out []byte
	switch n {
	case 128:
		out = make([]byte, 18)
		if a.Is4() {
			binary.LittleEndian.PutUint16(out, afInet4)
			b := a.As4()
			copy(out[2:], b[:])
			out = out[:6]
		} else {
			binary.LittleEndian.PutUint16(out, afInet6)
			b := a.As16()
			copy(out[2:], b[:])
		}
	case 4:
		if !a.Is4() {
			return EAFNOSUPPORT
		}
		b := a.As4()
		out = b[:]
	case 16:
		b := a.As16()
		out = b[:]
	default:
		return EINVAL
	}
	if !m.Write(buf, out) {
		return EFAULT
	}
	return 0
}

func (p *Proc) newSock(s *sock, resultPtr uint32, m Memory) Result {
	if len(p.fds) >= MaxFDs {
		return fail(EMFILE)
	}
	fd := p.next
	for p.fds[fd] != nil {
		fd++
	}
	if !writeU32(m, resultPtr, fd) {
		return fail(EFAULT)
	}
	p.next = fd + 1
	p.fds[fd] = &fdEntry{kind: kindSocket, sock: s, read: true, write: true}
	return ok()
}

func (p *Proc) sockOpen(af, typ, resultPtr uint32, m Memory) Result {
	if p.cfg.Net == nil {
		return fail(ENOTSUP) // no network (--network none)
	}
	switch af {
	case afInet4, afInet6:
	case afUnix:
		return fail(EAFNOSUPPORT)
	default:
		return fail(EINVAL)
	}
	switch typ {
	case sockDgram, sockStream:
	case sockAny:
		typ = sockStream
	default:
		return fail(EINVAL)
	}
	return p.newSock(&sock{stype: uint8(typ), family: uint8(af)}, resultPtr, m)
}

// ownAddr accepts a local address to bind: unspecified, loopback or the
// endpoint's own.
func (p *Proc) ownAddr(a netip.Addr) bool {
	return a.IsUnspecified() || a.IsLoopback() || a == p.cfg.Net.Addr()
}

func (p *Proc) sockBind(fd, addrPtr, port uint32, m Memory) Result {
	s, e := p.sockOf(fd)
	if e != 0 {
		return fail(e)
	}
	a, e := readAddr(m, addrPtr)
	if e != 0 {
		return fail(e)
	}
	if port > 65535 {
		return fail(EINVAL)
	}
	if v := p.view(s); s.bound || v.conn != nil || v.connecting {
		return fail(EINVAL)
	}
	if !p.ownAddr(a) {
		return fail(EADDRNOTAVAIL)
	}
	s.bound, s.port = true, uint16(port)
	s.local = netip.AddrPortFrom(p.cfg.Net.Addr(), uint16(port))
	if s.stype == sockStream {
		return ok() // the driver binds and listens in one step: at sock_listen
	}
	var pc net.PacketConn
	var err error
	return p.async(func() {
		pc, err = p.cfg.Net.ListenPacket(p.net.ctx, "udp", uint16(port))
	}, func(Memory) Result {
		if err != nil {
			s.bound = false
			return fail(sockErrno(err))
		}
		p.startPacket(s, pc, nil)
		return ok()
	})
}

func (p *Proc) sockListen(fd uint32) Result {
	s, e := p.sockOf(fd)
	if e != 0 {
		return fail(e)
	}
	if s.stype != sockStream {
		return fail(ENOTSUP)
	}
	v := p.view(s)
	if v.conn != nil || v.connecting {
		return fail(EINVAL)
	}
	if v.listener != nil {
		return ok()
	}
	var l net.Listener
	var err error
	return p.async(func() {
		l, err = p.cfg.Net.Listen(p.net.ctx, "tcp", s.port)
	}, func(Memory) Result {
		if err != nil {
			return fail(sockErrno(err))
		}
		n := p.net
		n.mu.Lock()
		if s.closed {
			n.mu.Unlock()
			l.Close()
			return fail(EBADF)
		}
		s.listener = l
		if ta, ok := l.Addr().(*net.TCPAddr); ok {
			s.local = netip.AddrPortFrom(p.cfg.Net.Addr(), uint16(ta.Port))
		}
		n.mu.Unlock()
		go p.acceptLoop(s, l)
		return ok()
	})
}

// maxAccepted bounds the connections accepted from the driver that the
// guest has not taken yet (the driver's own backlog holds the rest).
const maxAccepted = 64

func (p *Proc) acceptLoop(s *sock, l net.Listener) {
	n := p.net
	for {
		n.mu.Lock()
		for len(s.accepted) >= maxAccepted && !s.closed {
			ch := n.wake
			n.mu.Unlock()
			<-ch
			n.mu.Lock()
		}
		closed := s.closed
		n.mu.Unlock()
		if closed {
			return
		}
		c, err := l.Accept()
		n.mu.Lock()
		if s.closed {
			n.mu.Unlock()
			if c != nil {
				c.Close()
			}
			return
		}
		if err != nil {
			s.aErr = err
			n.changed()
			n.mu.Unlock()
			return
		}
		s.accepted = append(s.accepted, c)
		n.changed()
		n.mu.Unlock()
	}
}

func addrPortOf(a net.Addr) netip.AddrPort {
	switch v := a.(type) {
	case *net.TCPAddr:
		return v.AddrPort()
	case *net.UDPAddr:
		return v.AddrPort()
	}
	if ap, err := netip.ParseAddrPort(a.String()); err == nil {
		return ap
	}
	return netip.AddrPort{}
}

func unmapAP(ap netip.AddrPort) netip.AddrPort {
	return netip.AddrPortFrom(ap.Addr().Unmap(), ap.Port())
}

func (p *Proc) sockAccept(fd, flags, resultPtr uint32, m Memory) Result {
	s, e := p.sockOf(fd)
	if e != 0 {
		return fail(e)
	}
	n := p.net
	n.mu.Lock()
	if s.listener == nil {
		n.mu.Unlock()
		return fail(EINVAL)
	}
	if len(s.accepted) == 0 {
		err, wake := s.aErr, n.wake
		n.mu.Unlock()
		if err != nil {
			return fail(sockErrno(err))
		}
		return p.block(s, wake, func(m Memory) Result { return p.sockAccept(fd, flags, resultPtr, m) })
	}
	c := s.accepted[0]
	s.accepted = s.accepted[1:]
	n.changed()
	n.mu.Unlock()
	ns := &sock{stype: sockStream, family: s.family, nonblock: flags&fdflagNonblock != 0}
	if r := p.newSock(ns, resultPtr, m); r.Errno != 0 {
		c.Close()
		return r
	}
	p.startStream(ns, c)
	return ok()
}

// startStream runs a connected stream's reader and writer.
func (p *Proc) startStream(s *sock, c net.Conn) {
	n := p.net
	n.mu.Lock()
	s.conn = c
	s.local = unmapAP(addrPortOf(c.LocalAddr()))
	s.peer = unmapAP(addrPortOf(c.RemoteAddr()))
	n.changed()
	n.mu.Unlock()
	go p.streamReader(s, c)
	go p.streamWriter(s, c)
}

func (p *Proc) streamReader(s *sock, c net.Conn) {
	n := p.net
	buf := make([]byte, 16<<10)
	for {
		n.mu.Lock()
		for len(s.rbuf) >= sockBuf && !s.closed && !s.rShut {
			ch := n.wake
			n.mu.Unlock()
			<-ch
			n.mu.Lock()
		}
		stop := s.closed
		n.mu.Unlock()
		if stop {
			return
		}
		k, err := c.Read(buf)
		n.mu.Lock()
		if k > 0 && !s.rShut {
			s.rbuf = append(s.rbuf, buf[:k]...)
		}
		if err == io.EOF {
			s.rEOF = true
		} else if err != nil {
			s.rErr = err
		}
		n.changed()
		n.mu.Unlock()
		if err != nil {
			return
		}
	}
}

func (p *Proc) streamWriter(s *sock, c net.Conn) {
	n := p.net
	for {
		n.mu.Lock()
		for len(s.wbuf) == 0 && !s.closed && !s.wShut {
			ch := n.wake
			n.mu.Unlock()
			<-ch
			n.mu.Lock()
		}
		if s.closed {
			n.mu.Unlock()
			return
		}
		if len(s.wbuf) == 0 && s.wShut {
			n.mu.Unlock()
			if cw, ok := c.(interface{ CloseWrite() error }); ok {
				cw.CloseWrite()
			}
			return
		}
		chunk := append([]byte(nil), s.wbuf...)
		n.mu.Unlock()
		k, err := c.Write(chunk)
		n.mu.Lock()
		s.wbuf = s.wbuf[k:]
		if len(s.wbuf) == 0 {
			s.wbuf = nil
		}
		if err != nil {
			s.wErr = err
		}
		n.changed()
		n.mu.Unlock()
		if err != nil {
			return
		}
	}
}

// startPacket runs a datagram socket's reader and sender; conn is set
// for a connected one (reads then come from its peer only).
func (p *Proc) startPacket(s *sock, pc net.PacketConn, conn net.Conn) {
	n := p.net
	n.mu.Lock()
	s.pc = pc
	if conn != nil {
		s.conn = conn
		s.local = unmapAP(addrPortOf(conn.LocalAddr()))
		s.peer = unmapAP(addrPortOf(conn.RemoteAddr()))
	} else {
		s.local = unmapAP(addrPortOf(pc.LocalAddr()))
		s.local = netip.AddrPortFrom(p.cfg.Net.Addr(), s.local.Port())
	}
	n.changed()
	n.mu.Unlock()
	go p.packetReader(s, pc, conn)
	go p.packetSender(s, pc, conn)
}

func (p *Proc) packetReader(s *sock, pc net.PacketConn, conn net.Conn) {
	n := p.net
	buf := make([]byte, netdriver.MaxDatagram)
	for {
		var k int
		var from netip.AddrPort
		var err error
		if conn != nil {
			k, err = conn.Read(buf)
			from = s.peer
		} else {
			var a net.Addr
			k, a, err = pc.ReadFrom(buf)
			if a != nil {
				from = unmapAP(addrPortOf(a))
			}
		}
		n.mu.Lock()
		if s.closed {
			n.mu.Unlock()
			return
		}
		if err != nil {
			s.rErr = err
			n.changed()
			n.mu.Unlock()
			return
		}
		if len(s.dgrams) < netdriver.DatagramQueue && !s.rShut {
			s.dgrams = append(s.dgrams, dgram{from, append([]byte(nil), buf[:k]...)})
			n.changed()
		}
		n.mu.Unlock()
	}
}

func (p *Proc) packetSender(s *sock, pc net.PacketConn, conn net.Conn) {
	n := p.net
	for {
		n.mu.Lock()
		for len(s.dq) == 0 && !s.closed {
			ch := n.wake
			n.mu.Unlock()
			<-ch
			n.mu.Lock()
		}
		if s.closed {
			n.mu.Unlock()
			return
		}
		d := s.dq[0]
		s.dq = s.dq[1:]
		n.changed()
		n.mu.Unlock()
		var err error
		switch {
		case conn != nil && (!d.addr.IsValid() || d.addr == s.peer):
			_, err = conn.Write(d.b)
		case pc != nil:
			_, err = pc.WriteTo(d.b, net.UDPAddrFromAddrPort(d.addr))
		default:
			err = syscall.EISCONN // a connected socket whose driver sends to its peer only
		}
		if err != nil {
			n.mu.Lock()
			s.wErr = err // reported by the next send, as an ICMP error would be
			n.changed()
			n.mu.Unlock()
		}
	}
}

func (p *Proc) sockConnect(fd, addrPtr, port uint32, m Memory) Result {
	s, e := p.sockOf(fd)
	if e != 0 {
		return fail(e)
	}
	a, e := readAddr(m, addrPtr)
	if e != 0 {
		return fail(e)
	}
	if port > 65535 {
		return fail(EINVAL)
	}
	dst := netip.AddrPortFrom(a, uint16(port))
	n := p.net
	n.mu.Lock()
	if s.listener != nil {
		n.mu.Unlock()
		return fail(EINVAL)
	}
	if s.stype == sockDgram {
		if s.pc != nil {
			s.peer = dst // a bound socket: filter and default destination
			n.mu.Unlock()
			return ok()
		}
		n.mu.Unlock()
		var c net.Conn
		var err error
		return p.async(func() {
			c, err = p.cfg.Net.Dial(p.net.ctx, "udp", dst)
		}, func(Memory) Result {
			if err != nil {
				return fail(sockErrno(err))
			}
			pc, _ := c.(net.PacketConn)
			p.startPacket(s, pc, c)
			return ok()
		})
	}
	switch {
	case s.connecting:
		n.mu.Unlock()
		return fail(EALREADY)
	case s.conn != nil:
		n.mu.Unlock()
		return fail(EISCONN)
	}
	s.connecting = true
	n.mu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := p.cfg.Net.Dial(p.net.ctx, "tcp", dst)
		n := p.net
		n.mu.Lock()
		s.connecting = false
		if err == nil && s.closed {
			err = net.ErrClosed
			c.Close()
		}
		s.soErr = err
		n.changed()
		n.mu.Unlock()
		if err == nil {
			p.startStream(s, c)
		}
	}()
	if s.nonblock {
		return fail(EINPROGRESS)
	}
	return Result{Wait: done, Resume: func(Memory) Result {
		p.net.mu.Lock()
		err := s.soErr
		s.soErr = nil
		p.net.mu.Unlock()
		return fail(sockErrno(err))
	}}
}

// recv reads into the guest's iovecs: a stream's buffered bytes, or one
// datagram (truncated to fit). It returns the count, whether a datagram
// was truncated, and its source.
func (p *Proc) recv(fd, iovs, niov, flags uint32, m Memory, onData func(n uint32, trunc bool, from netip.AddrPort) Result) Result {
	s, e := p.sockOf(fd)
	if e != 0 {
		return fail(e)
	}
	if niov > 1024 {
		return fail(EINVAL)
	}
	vec, okv := m.Read(iovs, niov*8)
	if !okv {
		return fail(EFAULT)
	}
	vec = append([]byte(nil), vec...)
	scatter := func(data []byte) (uint32, uint32) {
		var total uint32
		for i := uint32(0); i < niov && len(data) > 0; i++ {
			ptr, ln := le32(vec[i*8:]), le32(vec[i*8+4:])
			k := min(int(ln), len(data))
			if k > 0 && !m.Write(ptr, data[:k]) {
				return total, EFAULT
			}
			data = data[k:]
			total += uint32(k)
		}
		return total, 0
	}
	n := p.net
	n.mu.Lock()
	wake := n.wake
	if s.stype == sockDgram {
		if len(s.dgrams) == 0 {
			err, shut, unbound := s.rErr, s.rShut, s.pc == nil
			n.mu.Unlock()
			switch {
			case shut:
				return onData(0, false, netip.AddrPort{})
			case err != nil:
				return fail(sockErrno(err))
			case unbound:
				return fail(ENOTCONN)
			}
			return p.block(s, wake, func(m Memory) Result { return p.recv(fd, iovs, niov, flags, m, onData) })
		}
		d := s.dgrams[0]
		if flags&riPeek == 0 {
			s.dgrams = s.dgrams[1:]
			n.changed()
		}
		n.mu.Unlock()
		k, e := scatter(d.b)
		if e != 0 {
			return fail(e)
		}
		return onData(k, int(k) < len(d.b), d.addr)
	}
	if s.conn == nil {
		connecting := s.connecting
		n.mu.Unlock()
		if connecting {
			return p.block(s, wake, func(m Memory) Result { return p.recv(fd, iovs, niov, flags, m, onData) })
		}
		return fail(ENOTCONN)
	}
	peer := s.peer
	if len(s.rbuf) == 0 {
		eof, err, shut := s.rEOF, s.rErr, s.rShut
		n.mu.Unlock()
		switch {
		case eof || shut:
			return onData(0, false, peer)
		case err != nil:
			return fail(sockErrno(err))
		}
		return p.block(s, wake, func(m Memory) Result { return p.recv(fd, iovs, niov, flags, m, onData) })
	}
	data := s.rbuf
	n.mu.Unlock()
	k, e := scatter(data)
	if e != 0 {
		return fail(e)
	}
	if flags&riPeek == 0 {
		n.mu.Lock()
		s.rbuf = s.rbuf[k:]
		if len(s.rbuf) == 0 {
			s.rbuf = nil
		}
		n.changed()
		n.mu.Unlock()
	}
	return onData(k, false, peer)
}

// recvDone writes a receive's count and output flags.
func (p *Proc) recvDone(k uint32, trunc bool, nPtr, flagsPtr uint32, m Memory) Result {
	var fl uint16
	if trunc {
		fl = roDataTruncated
	}
	if !writeU32(m, nPtr, k) {
		return fail(EFAULT)
	}
	// ro_flags is a u16 in preview 1; stealthrocket/net passes an int32
	// it zeroed: two bytes suit both.
	var b [2]byte
	binary.LittleEndian.PutUint16(b[:], fl)
	if !m.Write(flagsPtr, b[:]) {
		return fail(EFAULT)
	}
	return ok()
}

// gather reads the guest's iovecs, at most limit bytes.
func gather(iovs, niov uint32, limit int, m Memory) ([]byte, uint32) {
	if niov > 1024 {
		return nil, EINVAL
	}
	vec, okv := m.Read(iovs, niov*8)
	if !okv {
		return nil, EFAULT
	}
	vec = append([]byte(nil), vec...)
	var out []byte
	for i := uint32(0); i < niov && len(out) < limit; i++ {
		ptr, ln := le32(vec[i*8:]), le32(vec[i*8+4:])
		ln = uint32(min(int(ln), limit-len(out)))
		if ln == 0 {
			continue
		}
		b, ok := m.Read(ptr, ln)
		if !ok {
			return nil, EFAULT
		}
		out = append(out, b...)
	}
	return out, 0
}

// send queues the guest's bytes: on a stream as much as the buffer takes,
// a whole datagram (to dst, or the connected peer) otherwise.
func (p *Proc) send(fd, iovs, niov uint32, dst netip.AddrPort, resultPtr uint32, m Memory) Result {
	s, e := p.sockOf(fd)
	if e != 0 {
		return fail(e)
	}
	n := p.net
	wake := n.current()
	v := p.view(s)
	if s.stype == sockDgram {
		// Sizing first: an iovec list may name more than a datagram holds.
		data, e := gather(iovs, niov, netdriver.MaxDatagram+1, m)
		if e != 0 {
			return fail(e)
		}
		if len(data) > netdriver.MaxDatagram {
			return fail(EMSGSIZE)
		}
		if !dst.IsValid() || dst.Addr().IsUnspecified() && dst.Port() == 0 {
			if !v.peer.IsValid() {
				return fail(ENOTCONN) // EDESTADDRREQ on Linux
			}
			dst = v.peer
		}
		if v.pc == nil && v.conn == nil {
			// An unbound socket sends from an ephemeral port, as bind(0).
			var pc net.PacketConn
			var err error
			return p.async(func() {
				pc, err = p.cfg.Net.ListenPacket(p.net.ctx, "udp", 0)
			}, func(m Memory) Result {
				if err != nil {
					return fail(sockErrno(err))
				}
				p.startPacket(s, pc, nil)
				return p.send(fd, iovs, niov, dst, resultPtr, m)
			})
		}
		n.mu.Lock()
		if err := s.wErr; err != nil {
			s.wErr = nil
			n.mu.Unlock()
			return fail(sockErrno(err))
		}
		if len(s.dq) >= netdriver.DatagramQueue {
			n.mu.Unlock()
			return p.block(s, wake, func(m Memory) Result { return p.send(fd, iovs, niov, dst, resultPtr, m) })
		}
		s.dq = append(s.dq, dgram{dst, data})
		n.changed()
		n.mu.Unlock()
		if !writeU32(m, resultPtr, uint32(len(data))) {
			return fail(EFAULT)
		}
		return ok()
	}
	if v.conn == nil {
		if v.connecting {
			return p.block(s, wake, func(m Memory) Result { return p.send(fd, iovs, niov, dst, resultPtr, m) })
		}
		return fail(ENOTCONN)
	}
	n.mu.Lock()
	room := sockBuf - len(s.wbuf)
	err, shut := s.wErr, s.wShut
	n.mu.Unlock()
	switch {
	case shut:
		return fail(EPIPE)
	case err != nil:
		return fail(sockErrno(err))
	case room <= 0:
		return p.block(s, wake, func(m Memory) Result { return p.send(fd, iovs, niov, dst, resultPtr, m) })
	}
	data, e := gather(iovs, niov, room, m)
	if e != 0 {
		return fail(e)
	}
	n.mu.Lock()
	s.wbuf = append(s.wbuf, data...)
	n.changed()
	n.mu.Unlock()
	if !writeU32(m, resultPtr, uint32(len(data))) {
		return fail(EFAULT)
	}
	return ok()
}

func (p *Proc) sockShutdown(fd, how uint32) Result {
	s, e := p.sockOf(fd)
	if e != 0 {
		return fail(e)
	}
	if how == 0 || how&^(shutRd|shutWr) != 0 {
		return fail(EINVAL)
	}
	n := p.net
	n.mu.Lock()
	if s.conn == nil && s.pc == nil {
		n.mu.Unlock()
		return fail(ENOTCONN)
	}
	if how&shutRd != 0 {
		s.rShut = true
		s.rbuf = nil
		s.dgrams = nil
	}
	if how&shutWr != 0 {
		s.wShut = true
	}
	n.changed()
	n.mu.Unlock()
	return ok()
}

func (p *Proc) sockGetOpt(fd, level, name, valPtr, valLen uint32, m Memory) Result {
	s, e := p.sockOf(fd)
	if e != 0 {
		return fail(e)
	}
	if level != 0 {
		return fail(ENOPROTOOPT)
	}
	if valLen != 4 {
		return fail(EINVAL)
	}
	var v uint32
	switch name {
	case soError:
		p.net.mu.Lock()
		v = sockErrno(s.soErr)
		s.soErr = nil
		p.net.mu.Unlock()
	case soType:
		v = uint32(s.stype)
	case soReuseAddr:
		if s.reuse {
			v = 1
		}
	case soAcceptConn:
		if p.view(s).listener != nil {
			v = 1
		}
	case soSndBuf, soRcvBuf:
		v = sockBuf
	case soBroadcast, soKeepAlive:
		v = 0
	default:
		return fail(ENOPROTOOPT)
	}
	if !writeU32(m, valPtr, v) {
		return fail(EFAULT)
	}
	return ok()
}

func (p *Proc) sockSetOpt(fd, level, name, valPtr, valLen uint32, m Memory) Result {
	s, e := p.sockOf(fd)
	if e != 0 {
		return fail(e)
	}
	if level != 0 {
		return fail(ENOPROTOOPT)
	}
	if valLen != 4 {
		return fail(EINVAL)
	}
	v, ok := readU32(m, valPtr)
	if !ok {
		return fail(EFAULT)
	}
	switch name {
	case soReuseAddr:
		s.reuse = v != 0
	case soBroadcast, soKeepAlive, soSndBuf, soRcvBuf:
		// accepted; the network has no broadcast and fixed buffers
	default:
		return fail(ENOPROTOOPT)
	}
	return Result{}
}

func (p *Proc) sockAddr(fd, addrPtr, portPtr uint32, peer bool, m Memory) Result {
	s, e := p.sockOf(fd)
	if e != 0 {
		return fail(e)
	}
	p.net.mu.Lock()
	ap := s.local
	if peer {
		ap = s.peer
	}
	p.net.mu.Unlock()
	if peer && !ap.IsValid() {
		return fail(ENOTCONN)
	}
	if !ap.IsValid() {
		ap = netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
	}
	if e := writeAddr(m, addrPtr, ap.Addr()); e != 0 {
		return fail(e)
	}
	if !writeU32(m, portPtr, uint32(ap.Port())) {
		return fail(EFAULT)
	}
	return ok()
}

// services is what getaddrinfo knows by name.
var services = map[string]uint16{"http": 80, "https": 443, "domain": 53, "ssh": 22, "ftp": 21, "smtp": 25, "ntp": 123}

func cstring(m Memory, ptr, n, max uint32) (string, bool) {
	if n > max {
		return "", false
	}
	b, ok := m.Read(ptr, n)
	if !ok {
		return "", false
	}
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return string(b), true
}

// sockGetAddrInfo resolves node and service into the guest's preallocated
// chain of __wasi_addrinfo_t (each pointing at a __wasi_sockaddr_t whose
// sa_data receives the port, big-endian, then the address).
func (p *Proc) sockGetAddrInfo(nodePtr, nodeLen, servPtr, servLen, hintsPtr, resPtr, maxRes, resLenPtr uint32, m Memory) Result {
	if p.cfg.Net == nil {
		return fail(ENOTSUP)
	}
	node, ok1 := cstring(m, nodePtr, nodeLen, 256)
	serv, ok2 := cstring(m, servPtr, servLen, 64)
	if !ok1 || !ok2 {
		return fail(EFAULT)
	}
	if node == "" && serv == "" {
		return fail(EINVAL)
	}
	hints, ok := m.Read(hintsPtr, 28)
	if !ok {
		return fail(EFAULT)
	}
	hflags := binary.LittleEndian.Uint16(hints)
	family, stype := hints[2], hints[3]
	var port uint16
	if serv != "" {
		if v, err := strconv.ParseUint(serv, 10, 16); err == nil {
			port = uint16(v)
		} else if v, ok := services[serv]; ok {
			port = v
		} else {
			return fail(EAI_SERVICE)
		}
	}
	finish := func(addrs []netip.Addr, m Memory) Result {
		var keep []netip.Addr
		for _, a := range addrs {
			if family == afInet4 && !a.Is4() || family == afInet6 && a.Is4() {
				continue
			}
			keep = append(keep, a)
		}
		if len(keep) == 0 {
			return fail(EAI_NONAME)
		}
		if stype == sockAny {
			stype = sockStream
		}
		proto := uint32(ipprotoTCP)
		if stype == sockDgram {
			proto = ipprotoUDP
		}
		cur, ok := readU32(m, resPtr)
		if !ok {
			return fail(EFAULT)
		}
		var count uint32
		for _, a := range keep {
			if count >= maxRes || cur == 0 {
				break
			}
			ai, ok := m.Read(cur, 28)
			if !ok {
				return fail(EFAULT)
			}
			ai = append([]byte(nil), ai...)
			saPtr, next := le32(ai[12:]), le32(ai[24:])
			sa, ok := m.Read(saPtr, 12)
			if !ok {
				return fail(EFAULT)
			}
			dataLen, dataPtr := le32(sa[4:]), le32(sa[8:])
			ab := a.AsSlice()
			if dataLen < uint32(2+len(ab)) {
				return fail(EFAULT)
			}
			data := make([]byte, 2+len(ab))
			binary.BigEndian.PutUint16(data, port)
			copy(data[2:], ab)
			fam := byte(afInet4)
			if !a.Is4() {
				fam = afInet6
			}
			if !m.Write(dataPtr, data) || !m.Write(saPtr, []byte{fam}) {
				return fail(EFAULT)
			}
			binary.LittleEndian.PutUint16(ai[0:], 0)
			ai[2], ai[3] = fam, stype
			put32(ai[4:], proto)
			put32(ai[8:], 12) // sizeof(__wasi_sockaddr_t)
			put32(ai[20:], 0) // no canonical name
			if !m.Write(cur, ai[:24]) {
				return fail(EFAULT)
			}
			count++
			cur = next
		}
		if !writeU32(m, resLenPtr, count) {
			return fail(EFAULT)
		}
		return Result{}
	}
	switch {
	case node == "" && hflags&aiPassive != 0:
		return finish([]netip.Addr{netip.IPv4Unspecified()}, m)
	case node == "":
		return finish([]netip.Addr{netip.AddrFrom4([4]byte{127, 0, 0, 1})}, m)
	}
	if a, err := netip.ParseAddr(node); err == nil {
		return finish([]netip.Addr{a.Unmap()}, m)
	}
	if hflags&aiNumericHost != 0 {
		return fail(EAI_NONAME)
	}
	var addrs []netip.Addr
	var err error
	return p.async(func() {
		ctx, cancel := context.WithTimeout(p.net.ctx, 10*time.Second)
		defer cancel()
		addrs, err = p.cfg.Net.Resolve(ctx, node)
	}, func(m Memory) Result {
		if err != nil {
			var d *net.DNSError
			if errors.As(err, &d) && d.IsNotFound {
				return fail(EAI_NONAME)
			}
			return fail(sockErrno(err))
		}
		return finish(addrs, m)
	})
}

// sockReady reports a socket's poll readiness for reading or writing, and
// whether it hung up.
func (p *Proc) sockReady(s *sock, write bool) (ready, hangup bool, nbytes uint64) {
	n := p.net
	n.mu.Lock()
	defer n.mu.Unlock()
	if s.closed {
		return true, true, 0
	}
	if write {
		switch {
		case s.stype == sockDgram:
			return len(s.dq) < netdriver.DatagramQueue || s.wErr != nil, false, sockBuf
		case s.connecting:
			return false, false, 0
		case s.conn != nil:
			room := sockBuf - len(s.wbuf)
			return room > 0 || s.wErr != nil || s.wShut, s.wErr != nil, uint64(max(room, 0))
		case s.soErr != nil:
			return true, true, 0
		}
		return false, false, 0
	}
	switch {
	case s.listener != nil:
		return len(s.accepted) > 0 || s.aErr != nil, s.aErr != nil, uint64(len(s.accepted))
	case s.stype == sockDgram:
		if len(s.dgrams) > 0 {
			return true, false, uint64(len(s.dgrams[0].b))
		}
		return s.rErr != nil || s.rShut, s.rErr != nil, 0
	case s.conn != nil:
		if len(s.rbuf) > 0 {
			return true, false, uint64(len(s.rbuf))
		}
		return s.rEOF || s.rErr != nil || s.rShut, s.rEOF || s.rErr != nil, 0
	case s.connecting:
		return false, false, 0
	}
	return s.soErr != nil, s.soErr != nil, 0
}
