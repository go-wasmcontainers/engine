// Package netdriver is how containers reach a network: a NetworkDriver
// gives each container an Endpoint, and the engine's WASI host maps the
// WasmEdge socket functions of wasi_snapshot_preview1 (sock_open,
// sock_bind, sock_listen, sock_accept, sock_connect, sock_recv_from,
// sock_send_to, sock_getaddrinfo, ...) onto it.
//
// The contract is the relay's (github.com/go-wasmcontainers/relay,
// docs/NETWORKDRIVER.md): addresses are the driver's, a connect blocks
// until the peer accepts or refuses, errors unwrap to the syscall.Errno a
// socket call returns. Local is an in-engine network with DNS by
// container name; package relaynet puts containers on a class network
// through the relay.
package netdriver

import (
	"context"
	"net"
	"net/netip"
)

// NetworkDriver gives containers endpoints on a network.
type NetworkDriver interface {
	// Attach gives container (a DNS label: [a-z0-9-]{1,63}) an endpoint.
	// Attaching the same name again returns the same address.
	Attach(ctx context.Context, container string) (Endpoint, error)
	Close() error
}

// Endpoint is one container's place on a network.
type Endpoint interface {
	Addr() netip.Addr
	// Listen is a SOCK_STREAM bind+listen ("tcp"); port 0 picks an
	// ephemeral port, which the listener's Addr says.
	Listen(ctx context.Context, network string, port uint16) (net.Listener, error)
	// Dial is a SOCK_STREAM connect ("tcp") or a connected SOCK_DGRAM
	// socket ("udp").
	Dial(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error)
	// ListenPacket is a SOCK_DGRAM bind ("udp").
	ListenPacket(ctx context.Context, network string, port uint16) (net.PacketConn, error)
	// Resolve is sock_getaddrinfo: a container's name, or an IP literal.
	Resolve(ctx context.Context, name string) ([]netip.Addr, error)
	Detach() error
}

// ValidName reports whether s is a DNS label a container may be attached
// under: [a-z0-9-]{1,63}, not starting or ending with '-'.
func ValidName(s string) bool {
	if len(s) == 0 || len(s) > 63 || s[0] == '-' || s[len(s)-1] == '-' {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}
