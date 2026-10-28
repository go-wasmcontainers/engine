package container

import (
	"context"
	"fmt"
	"net/netip"
	"strings"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// Attachment is a network a container joins and the extra names it
// answers to there (run --network-alias, network connect --alias).
type Attachment struct {
	// Network is the network's name; "" is the engine's default.
	Network string   `json:"network"`
	Aliases []string `json:"aliases,omitempty"`
}

// NoNetwork is the network that is none.
const NoNetwork = "none"

// MaxAttachments bounds the networks one container joins (each datagram
// socket it opens costs a reader per network: see netdriver.Multi).
const MaxAttachments = 8

// NetworkInfo is a container's place on one network, as inspect shows it.
type NetworkInfo struct {
	Network string
	Aliases []string
	// IPAddress is its address there while it runs.
	IPAddress netip.Addr
}

// attachments is what cfg joins: its Networks, or the default network.
func (cfg Config) attachments() []Attachment {
	if len(cfg.Networks) == 0 {
		return []Attachment{{}}
	}
	return cfg.Networks
}

// PrimaryNetwork is the first network the container joins ("" = the
// engine's default, NoNetwork = none).
func (cfg Config) PrimaryNetwork() string { return cfg.attachments()[0].Network }

// normAliases lower-cases aliases (DNS names do not have a case) and
// refuses those that are not DNS labels.
func normAliases(aliases []string) ([]string, error) {
	if len(aliases) > netdriver.MaxAliases {
		return nil, fmt.Errorf("%w: at most %d aliases per network", ErrInvalid, netdriver.MaxAliases)
	}
	var out []string
	for _, a := range aliases {
		a = strings.ToLower(a)
		if !netdriver.ValidName(a) {
			return nil, fmt.Errorf("%w: invalid network alias %q (a DNS label: letters, digits, '-')", ErrInvalid, a)
		}
		out = append(out, a)
	}
	return out, nil
}

// checkAttachments validates and normalises a container's networks.
func checkAttachments(atts []Attachment) ([]Attachment, error) {
	if len(atts) > MaxAttachments {
		return nil, fmt.Errorf("%w: at most %d networks per container", ErrInvalid, MaxAttachments)
	}
	out := make([]Attachment, 0, len(atts))
	seen := map[string]bool{}
	for _, a := range atts {
		if a.Network != "" && !ValidNetworkName(a.Network) {
			return nil, fmt.Errorf("%w: invalid network name %q", ErrInvalid, a.Network)
		}
		if seen[a.Network] {
			return nil, fmt.Errorf("%w: network %q given twice", ErrInvalid, a.Network)
		}
		seen[a.Network] = true
		al, err := normAliases(a.Aliases)
		if err != nil {
			return nil, err
		}
		if a.Network == NoNetwork && (len(atts) > 1 || len(al) > 0) {
			return nil, fmt.Errorf("%w: container cannot be connected to multiple networks with one of the networks in private (none) mode", ErrInvalid)
		}
		out = append(out, Attachment{a.Network, al})
	}
	return out, nil
}

// UsesNetwork reports whether a container, in any state, joins a network.
func (m *Manager) UsesNetwork(name string) bool {
	m.mu.Lock()
	cs := make([]*Container, 0, len(m.byID))
	for _, c := range m.byID {
		cs = append(cs, c)
	}
	m.mu.Unlock()
	for _, c := range cs {
		c.mu.Lock()
		atts := c.cfg.attachments()
		c.mu.Unlock()
		for _, a := range atts {
			if a.Network == name {
				return true
			}
		}
	}
	return false
}

// attach puts a starting container on its networks; nil when it has
// none. The caller holds c.mu.
func (m *Manager) attach(ctx context.Context, c *Container) (*netdriver.Multi, error) {
	if m.opt.Network == nil || c.cfg.PrimaryNetwork() == NoNetwork {
		return nil, nil
	}
	mn := netdriver.NewMulti()
	for _, a := range c.cfg.attachments() {
		ep, err := m.opt.Network(ctx, a.Network, DNSName(c.name, c.id), a.Aliases)
		if err == nil && ep != nil {
			err = mn.Add(ctx, a.Network, ep)
		}
		if err != nil {
			mn.Detach()
			return nil, fmt.Errorf("%w: network %q: %v", ErrInvalid, a.Network, err)
		}
	}
	if len(mn.Endpoints()) == 0 {
		return nil, nil
	}
	return mn, nil
}

// Connect puts a container on another network (network connect). A
// running container is attached at once: its open listeners listen there
// too. Connecting it again to a network it is on replaces its aliases
// there only when it is not running (as Docker refuses it otherwise).
func (m *Manager) Connect(ctx context.Context, ref string, a Attachment) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	if a.Network != "" && !ValidNetworkName(a.Network) {
		return fmt.Errorf("%w: invalid network name %q", ErrInvalid, a.Network)
	}
	al, err := normAliases(a.Aliases)
	if err != nil {
		return err
	}
	a.Aliases = al
	c.mu.Lock()
	atts := append([]Attachment(nil), c.cfg.attachments()...)
	for _, x := range atts {
		if x.Network == a.Network {
			c.mu.Unlock()
			return fmt.Errorf("%w: endpoint with name %s already exists in network %s", ErrInvalid, c.name, a.Network)
		}
	}
	if atts[0].Network == NoNetwork {
		atts = nil // connecting a container on none replaces none
	}
	next, err := checkAttachments(append(atts, a))
	if err != nil {
		c.mu.Unlock()
		return err
	}
	if c.state == Running && c.net != nil {
		ep, err := m.opt.Network(ctx, a.Network, DNSName(c.name, c.id), a.Aliases)
		if err == nil && ep != nil {
			if err = c.net.Add(ctx, a.Network, ep); err != nil {
				ep.Detach()
			}
		}
		if err != nil {
			c.mu.Unlock()
			return fmt.Errorf("%w: network %q: %v", ErrInvalid, a.Network, err)
		}
	} else if c.state == Running {
		c.mu.Unlock()
		return fmt.Errorf("%w: the container started without a network; restart it to connect it", ErrInvalid)
	}
	c.cfg.Networks = next
	c.mu.Unlock()
	m.schedule(c)
	return nil
}

// Disconnect takes a container off a network (network disconnect); a
// container taken off its last network has none.
func (m *Manager) Disconnect(ref, network string) error {
	c, err := m.Get(ref)
	if err != nil {
		return err
	}
	c.mu.Lock()
	atts := c.cfg.attachments()
	i := -1
	for j, x := range atts {
		if x.Network == network {
			i = j
		}
	}
	if i < 0 || network == NoNetwork {
		c.mu.Unlock()
		return fmt.Errorf("%w: container %s is not connected to network %s", ErrInvalid, c.name, displayNetwork(network))
	}
	next := append(append([]Attachment(nil), atts[:i]...), atts[i+1:]...)
	if len(next) == 0 {
		next = []Attachment{{Network: NoNetwork}}
	}
	if c.state == Running && c.net != nil {
		if ep, ok := c.net.Remove(network); ok {
			ep.Detach()
		}
		c.ip = c.net.Addr()
	}
	c.cfg.Networks = next
	c.mu.Unlock()
	m.schedule(c)
	return nil
}

func displayNetwork(n string) string {
	if n == "" {
		return "the default network"
	}
	return n
}

// networksLocked is what inspect shows; the caller holds c.mu.
func (c *Container) networksLocked() []NetworkInfo {
	var live []netdriver.NamedEndpoint
	if c.state == Running && c.net != nil {
		live = c.net.Endpoints()
	}
	var out []NetworkInfo
	for _, a := range c.cfg.attachments() {
		if a.Network == NoNetwork {
			continue
		}
		ni := NetworkInfo{Network: a.Network, Aliases: a.Aliases}
		for _, e := range live {
			if e.Network == a.Network {
				ni.IPAddress = e.Endpoint.Addr()
			}
		}
		out = append(out, ni)
	}
	return out
}
