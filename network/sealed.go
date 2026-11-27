package network

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"syscall"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// ErrEgress is an internal network asked to carry traffic out of the engine:
// a driver that does not declare it cannot, or an endpoint that reports it
// is remote. Calls it refuses unwrap to syscall.ENETUNREACH as well.
var ErrEgress = errors.New("network: internal network has no route out")

// admit is the one test of an internal network: the driver must POSITIVELY
// declare that it cannot carry traffic out (netdriver.Confined). Unknown is
// refused, so a wrapper cannot launder a driver's egress.
func admit(name string, d netdriver.NetworkDriver) error {
	if !netdriver.Confined(d) {
		return fmt.Errorf("%w: network %s is internal and its driver does not declare that it has no egress: %w", ErrInvalid, name, ErrEgress)
	}
	return nil
}

// sealed is an endpoint on an internal network. Its driver is asked again at
// every call, and an endpoint that reports Remote is refused, so egress
// learnt after the network was added (a relay's Welcome) is caught.
type sealed struct {
	netdriver.Endpoint
	name string
	drv  netdriver.NetworkDriver
}

// sealedPrefixed keeps the Prefix of an endpoint that has one, which
// netdriver.Multi routes by.
type sealedPrefixed struct {
	sealed
	prefix interface{ Prefix() netip.Prefix }
}

func (s sealedPrefixed) Prefix() netip.Prefix { return s.prefix.Prefix() }

// seal returns ep refused unless its network is confined now.
func seal(name string, drv netdriver.NetworkDriver, ep netdriver.Endpoint) (netdriver.Endpoint, error) {
	s := sealed{ep, name, drv}
	if err := s.check(); err != nil {
		ep.Detach()
		return nil, err
	}
	if p, ok := ep.(interface{ Prefix() netip.Prefix }); ok {
		return sealedPrefixed{s, p}, nil
	}
	return s, nil
}

func (s sealed) check() error {
	if !netdriver.Confined(s.drv) || netdriver.IsRemote(s.Endpoint) {
		return fmt.Errorf("%w: network %s: %w", ErrEgress, s.name, syscall.ENETUNREACH)
	}
	return nil
}

func (s sealed) Listen(ctx context.Context, network string, port uint16) (net.Listener, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	return s.Endpoint.Listen(ctx, network, port)
}

func (s sealed) Dial(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	return s.Endpoint.Dial(ctx, network, addr)
}

func (s sealed) ListenPacket(ctx context.Context, network string, port uint16) (net.PacketConn, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	return s.Endpoint.ListenPacket(ctx, network, port)
}

func (s sealed) Resolve(ctx context.Context, name string) ([]netip.Addr, error) {
	if err := s.check(); err != nil {
		return nil, err
	}
	return s.Endpoint.Resolve(ctx, name)
}
