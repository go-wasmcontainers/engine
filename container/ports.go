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

// reconcilePorts re-decides, for a RUNNING container about to have the
// networks atts (network connect, disconnect), whether its ports are
// published: they are closed when every network becomes internal and opened
// again when one is not. The caller holds c.mu.
func (m *Manager) reconcilePorts(ctx context.Context, c *Container, atts []Attachment) error {
	if c.state != Running || c.net == nil {
		return nil
	}
	only := m.internalOnly(atts) || onNone(atts)
	switch {
	case only && c.ports != nil:
		c.ports.Close()
		c.ports = nil
	case !only && c.ports == nil:
		s, err := m.publish(ctx, c, c.net, atts)
		if err != nil {
			return err
		}
		c.ports = s
	}
	return nil
}

// onNone reports whether atts is the none network: Docker publishes nothing
// there.
func onNone(atts []Attachment) bool {
	for _, a := range atts {
		if a.Network == NoNetwork {
			return true
		}
	}
	return false
}

// internalOnly reports whether every network of atts is internal (and there
// is one).
func (m *Manager) internalOnly(atts []Attachment) bool {
	if m.opt.Internal == nil {
		return false
	}
	for _, a := range atts {
		if a.Network == NoNetwork || !m.opt.Internal(a.Network) {
			return false
		}
	}
	return len(atts) > 0
}

// publish opens a starting container's published ports, dialing into it
// through ep; nil when it publishes none. atts are the networks it is on.
// The caller holds c.mu.
func (m *Manager) publish(ctx context.Context, c *Container, ep *netdriver.Multi, atts []Attachment) (*portmap.Set, error) {
	want := c.cfg.published()
	if want.Bindings() == 0 {
		return nil, nil
	}
	if m.internalOnly(atts) {
		// Docker publishes nothing for a container on internal networks
		// only, silently: the ports stay exposed, none is listening.
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
