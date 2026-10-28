package container

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"

	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/portmap"
)

// ErrNoPublish is a container that asks for published ports from an engine
// that has nowhere to listen.
var ErrNoPublish = errors.New("this engine cannot publish ports")

// checkPorts validates a container's ports at create.
func checkPorts(cfg Config) error {
	if n := cfg.published().Bindings(); n > portmap.MaxBindings {
		return fmt.Errorf("%w: at most %d published ports", ErrInvalid, portmap.MaxBindings)
	}
	return nil
}

// publish opens a starting container's published ports, dialing into it
// through ep; nil when it publishes none. The caller holds c.mu.
func (m *Manager) publish(ctx context.Context, c *Container, ep *netdriver.Multi) (*portmap.Set, error) {
	want := c.cfg.published()
	if want.Bindings() == 0 {
		return nil, nil
	}
	if m.opt.Ports == nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalid, ErrNoPublish)
	}
	if ep == nil {
		// Docker publishes nothing for a container on none, silently; a
		// port that can never answer is better refused where it is asked.
		return nil, fmt.Errorf("%w: container %s has no network, so its ports cannot be published", ErrInvalid, c.name)
	}
	dial := func(ctx context.Context, network string, addr netip.AddrPort) (net.Conn, error) {
		return ep.Dial(ctx, network, addr)
	}
	s, err := portmap.Publish(ctx, m.opt.Ports, dial, want, m.opt.PortLimits)
	if err != nil {
		return nil, fmt.Errorf("driver failed programming external connectivity on endpoint %s: %w", c.name, err)
	}
	return s, nil
}
