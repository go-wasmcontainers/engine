package daemon

import (
	"context"
	"fmt"
	"sort"

	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/events"
	"github.com/go-wasmcontainers/engine/network"
)

// networkEvent publishes a network event as Docker does: the network's ID
// is the actor, its name and type are attributes (and the container's ID
// for connect and disconnect).
func (d *Daemon) networkEvent(action string, n network.Network, containerID string) {
	attrs := map[string]string{"name": n.Name, "type": n.Driver}
	if containerID != "" {
		attrs["container"] = containerID
	}
	d.events.Publish(events.Event{Type: events.Network, Action: action, ID: n.ID, Attributes: attrs})
}

// NetworkCreate makes an in-engine network.
func (d *Daemon) NetworkCreate(name string, labels map[string]string) (network.Network, error) {
	n, err := d.nets.Create(name, labels)
	if err == nil {
		d.networkEvent("create", n, "")
	}
	return n, err
}

// NetworkRemove removes a created network no container uses.
func (d *Daemon) NetworkRemove(ref string) error {
	n, err := d.nets.Get(ref)
	if err != nil {
		return err
	}
	if err := d.nets.Remove(n.Name, d.mgr.UsesNetwork); err != nil {
		return err
	}
	d.networkEvent("destroy", n, "")
	return nil
}

// NetworkPrune removes the created networks no container uses whose
// labels keep accepts (nil: all), and returns their names.
func (d *Daemon) NetworkPrune(keep func(labels map[string]string) bool) []string {
	byName := map[string]network.Network{}
	for _, n := range d.nets.List() {
		byName[n.Name] = n
	}
	gone := d.nets.Prune(d.mgr.UsesNetwork, keep)
	for _, g := range gone {
		d.networkEvent("destroy", byName[g], "")
	}
	d.events.Publish(events.Event{Type: events.Network, Action: "prune", Attributes: map[string]string{}})
	return gone
}

// attachmentName is how a container records a network: the default
// network as "", the others by name. "host" does not exist here.
func (d *Daemon) attachmentName(ref string) (string, error) {
	switch ref {
	case "", "default", network.Bridge:
		return "", nil
	case network.None:
		return container.NoNetwork, nil
	case network.Host:
		return "", fmt.Errorf("%w: the host network does not exist in a WebAssembly engine", container.ErrInvalid)
	}
	n, err := d.nets.Get(ref)
	if err != nil {
		return "", fmt.Errorf("%w: network %s not found", container.ErrInvalid, ref)
	}
	if n.Name == network.Bridge {
		return "", nil
	}
	return n.Name, nil
}

// attachments resolves a create's networks: the primary (NetworkMode),
// then the other networks EndpointsConfig names, by name, each with its
// aliases. With no NetworkMode, the first of those is the primary.
func (d *Daemon) attachments(mode string, endpoints map[string][]string) ([]container.Attachment, error) {
	aliases := map[string][]string{}
	var others []string
	for ref, al := range endpoints {
		name, err := d.attachmentName(ref)
		if err != nil {
			return nil, err
		}
		if _, dup := aliases[name]; dup {
			return nil, fmt.Errorf("%w: network %s named twice", container.ErrInvalid, ref)
		}
		aliases[name] = al
		others = append(others, name)
	}
	sort.Strings(others)
	primary, err := d.attachmentName(mode)
	if err != nil {
		return nil, err
	}
	if (mode == "" || mode == "default") && len(others) > 0 {
		primary = others[0]
	}
	atts := []container.Attachment{{Network: primary, Aliases: aliases[primary]}}
	for _, o := range others {
		if o != primary {
			atts = append(atts, container.Attachment{Network: o, Aliases: aliases[o]})
		}
	}
	for _, a := range atts {
		if a.Network == "" && len(a.Aliases) > 0 {
			return nil, errBridgeAliases
		}
	}
	if len(atts) == 1 && atts[0].Network == "" {
		return nil, nil // the default network
	}
	return atts, nil
}

var errBridgeAliases = fmt.Errorf("%w: network-scoped aliases are only supported for user-defined networks", container.ErrInvalid)

// NetworkConnect puts a container on a network (network connect).
func (d *Daemon) NetworkConnect(ctx context.Context, netRef, containerRef string, aliases []string) error {
	n, err := d.nets.Get(netRef)
	if err != nil {
		return err
	}
	name, err := d.attachmentName(n.Name)
	if err != nil {
		return err
	}
	if name == container.NoNetwork {
		return fmt.Errorf("%w: container cannot be connected to network none", container.ErrInvalid)
	}
	if name == "" && len(aliases) > 0 {
		return errBridgeAliases
	}
	c, err := d.mgr.Get(containerRef)
	if err != nil {
		return err
	}
	if err := d.mgr.Connect(ctx, c.ID(), container.Attachment{Network: name, Aliases: aliases}); err != nil {
		return err
	}
	d.networkEvent("connect", n, c.ID())
	return nil
}

// NetworkDisconnect takes a container off a network (network disconnect).
func (d *Daemon) NetworkDisconnect(netRef, containerRef string) error {
	n, err := d.nets.Get(netRef)
	if err != nil {
		return err
	}
	name, err := d.attachmentName(n.Name)
	if err != nil {
		return err
	}
	c, err := d.mgr.Get(containerRef)
	if err != nil {
		return err
	}
	if err := d.mgr.Disconnect(c.ID(), name); err != nil {
		return err
	}
	d.networkEvent("disconnect", n, c.ID())
	return nil
}
