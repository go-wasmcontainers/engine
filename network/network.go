// Package network names the networks containers join, as Docker's
// `network` commands do: "bridge" (the default) and the networks created
// by `wctr network create` are in-engine networks (netdriver.Local, with
// DNS by container name on every one of them, "bridge" included); "none"
// is no network; and drivers registered by the host — a class network
// through the relay — appear under their own names.
package network

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/state"
)

// Names the engine reserves.
const (
	Bridge = "bridge"
	None   = "none"
	Host   = "host"
)

var (
	// ErrNotFound is a network that does not exist.
	ErrNotFound = errors.New("network: not found")
	// ErrExists is a create of a name in use.
	ErrExists = errors.New("network: already exists")
	// ErrInUse is a remove of a network a container uses.
	ErrInUse = errors.New("network: in use")
	// ErrInvalid is a bad name or an operation a network refuses.
	ErrInvalid = errors.New("network: invalid")
)

var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,63}$`)

// MaxNetworks bounds the networks `network create` may make.
const MaxNetworks = 64

// Network is what `network ls` and `inspect` show.
type Network struct {
	Name    string
	ID      string
	Driver  string // "bridge" (in-engine), "null", or a host driver's name
	Subnet  string // empty for none and host drivers
	Created time.Time
	Labels  map[string]string
	// Builtin networks cannot be removed.
	Builtin bool
}

// Options configures a Manager.
type Options struct {
	// Store keeps created networks across restarts (nil: memory only).
	Store state.Store
	// Drivers are host-provided networks by name (e.g. "class" through the
	// relay). Their names are reserved.
	Drivers map[string]netdriver.NetworkDriver
	// DriverName describes a host driver in `network ls` (default
	// "remote").
	DriverName string
}

type entry struct {
	Network
	drv netdriver.NetworkDriver
}

// Manager holds the engine's networks.
type Manager struct {
	opt    Options
	mu     sync.Mutex
	by     map[string]*entry
	hosted []string // added by AddDriver
}

type record struct {
	Name    string            `json:"name"`
	ID      string            `json:"id"`
	Subnet  string            `json:"subnet"`
	Created time.Time         `json:"created"`
	Labels  map[string]string `json:"labels,omitempty"`
}

// New returns a manager with "bridge" and "none", the host drivers, and
// the networks created before (from Store); a damaged record is skipped
// and reported.
func New(opt Options) (*Manager, []string, error) {
	m := &Manager{opt: opt, by: map[string]*entry{}}
	br, err := netdriver.NewLocal(netdriver.DefaultPrefix)
	if err != nil {
		return nil, nil, err
	}
	epoch := time.Unix(0, 0).UTC()
	m.by[Bridge] = &entry{Network{Name: Bridge, ID: idFor(Bridge), Driver: "bridge", Subnet: netdriver.DefaultPrefix.String(), Created: epoch, Builtin: true}, br}
	m.by[None] = &entry{Network: Network{Name: None, ID: idFor(None), Driver: "null", Created: epoch, Builtin: true}}
	dn := opt.DriverName
	if dn == "" {
		dn = "remote"
	}
	for name, d := range opt.Drivers {
		if !nameRE.MatchString(name) || m.by[name] != nil || name == Host {
			return nil, nil, fmt.Errorf("%w: driver name %q", ErrInvalid, name)
		}
		m.by[name] = &entry{Network{Name: name, ID: idFor(name), Driver: dn, Created: epoch, Builtin: true}, d}
	}
	var repaired []string
	if opt.Store != nil {
		keys, err := opt.Store.Keys("network/")
		if err != nil {
			return nil, nil, err
		}
		sort.Strings(keys)
		for _, k := range keys {
			b, err := opt.Store.Get(k)
			var r record
			if err == nil {
				err = json.Unmarshal(b, &r)
			}
			var pfx netip.Prefix
			if err == nil {
				pfx, err = netip.ParsePrefix(r.Subnet)
			}
			if err == nil && (!nameRE.MatchString(r.Name) || m.by[r.Name] != nil || "network/"+keyName(r.Name) != k) {
				err = errors.New("bad name")
			}
			var drv *netdriver.Local
			if err == nil {
				drv, err = netdriver.NewLocal(pfx)
			}
			if err != nil {
				repaired = append(repaired, fmt.Sprintf("network record %s dropped: %v", k, err))
				opt.Store.Delete(k)
				continue
			}
			m.by[r.Name] = &entry{Network{Name: r.Name, ID: r.ID, Driver: "bridge", Subnet: pfx.String(), Created: r.Created, Labels: r.Labels}, drv}
		}
	}
	return m, repaired, nil
}

// keyName makes a state key's name part from a network name: keys are
// lower case and bounded (128 bytes), names are neither, so the name's
// SHA-256 (the record holds the name).
func keyName(name string) string {
	h := sha256.Sum256([]byte(name))
	return hex.EncodeToString(h[:])
}

func idFor(name string) string {
	b := make([]byte, 32)
	copy(b, name)
	return hex.EncodeToString(b)
}

func newID() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Default is the network a container joins when it names none.
func (m *Manager) Default() string { return Bridge }

// List returns the networks, by name.
func (m *Manager) List() []Network {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Network, 0, len(m.by))
	for _, e := range m.by {
		out = append(out, e.Network)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Get finds a network by name, ID or unique ID prefix.
func (m *Manager) Get(ref string) (Network, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.find(ref)
	if err != nil {
		return Network{}, err
	}
	return e.Network, nil
}

func (m *Manager) find(ref string) (*entry, error) {
	if e := m.by[ref]; e != nil {
		return e, nil
	}
	var found *entry
	if len(ref) >= 3 {
		for _, e := range m.by {
			if len(e.ID) >= len(ref) && e.ID[:len(ref)] == ref {
				if found != nil {
					return nil, fmt.Errorf("%w: %q is ambiguous", ErrInvalid, ref)
				}
				found = e
			}
		}
	}
	if found == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, ref)
	}
	return found, nil
}

// Create makes an in-engine network on the next free /16 of 10.89.0.0
// onwards.
func (m *Manager) Create(name string, labels map[string]string) (Network, error) {
	if !nameRE.MatchString(name) || name == Host {
		return Network{}, fmt.Errorf("%w: invalid network name %q", ErrInvalid, name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by[name] != nil {
		return Network{}, fmt.Errorf("%w: network with name %s already exists", ErrExists, name)
	}
	created := 0
	used := map[string]bool{}
	for _, e := range m.by {
		if !e.Builtin {
			created++
		}
		used[e.Subnet] = true
	}
	if created >= MaxNetworks {
		return Network{}, fmt.Errorf("%w: at most %d networks", ErrInvalid, MaxNetworks)
	}
	var pfx netip.Prefix
	for i := 89; i < 89+MaxNetworks+1; i++ {
		p := netip.PrefixFrom(netip.AddrFrom4([4]byte{10, byte(i), 0, 0}), 16)
		if !used[p.String()] {
			pfx = p
			break
		}
	}
	drv, err := netdriver.NewLocal(pfx)
	if err != nil {
		return Network{}, err
	}
	n := Network{Name: name, ID: newID(), Driver: "bridge", Subnet: pfx.String(), Created: time.Now().UTC(), Labels: labels}
	if m.opt.Store != nil {
		b, _ := json.Marshal(record{Name: n.Name, ID: n.ID, Subnet: n.Subnet, Created: n.Created, Labels: labels})
		if err := m.opt.Store.Put("network/"+keyName(name), b); err != nil {
			return Network{}, err
		}
	}
	m.by[name] = &entry{n, drv}
	return n, nil
}

// Remove deletes a created network that no container uses.
func (m *Manager) Remove(ref string, inUse func(name string) bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, err := m.find(ref)
	if err != nil {
		return err
	}
	if e.Builtin {
		return fmt.Errorf("%w: %s is a pre-defined network and cannot be removed", ErrInvalid, e.Name)
	}
	if inUse != nil && inUse(e.Name) {
		return fmt.Errorf("%w: network %s has active endpoints", ErrInUse, e.Name)
	}
	if m.opt.Store != nil {
		if err := m.opt.Store.Delete("network/" + keyName(e.Name)); err != nil {
			return err
		}
	}
	delete(m.by, e.Name)
	e.drv.Close()
	return nil
}

// Attach puts a container on a network ("" = the default) under its DNS
// name and its aliases; it is container.Options.Network. A network whose
// driver has no aliases refuses a container that asks for some.
func (m *Manager) Attach(ctx context.Context, network, name string, aliases []string) (netdriver.Endpoint, error) {
	if network == "" {
		network = m.Default()
	}
	m.mu.Lock()
	e, err := m.find(network)
	m.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if e.drv == nil || e.Name == Bridge { // Docker's rule: not on the default network
		if len(aliases) > 0 {
			return nil, fmt.Errorf("%w: network-scoped aliases are only supported for user-defined networks", ErrInvalid)
		}
		if e.drv == nil {
			return nil, nil
		}
	}
	if ad, ok := e.drv.(netdriver.AliasDriver); ok {
		return ad.AttachAliases(ctx, name, aliases)
	}
	if len(aliases) > 0 {
		return nil, fmt.Errorf("%w: network %s (%s) has no aliases", ErrInvalid, e.Name, e.Driver)
	}
	return e.drv.Attach(ctx, name)
}

// Prune removes every created network that no container uses and whose
// labels match keep (nil: all), and returns their names.
func (m *Manager) Prune(inUse func(name string) bool, keep func(labels map[string]string) bool) []string {
	m.mu.Lock()
	var names []string
	for name, e := range m.by {
		if !e.Builtin && (keep == nil || keep(e.Labels)) {
			names = append(names, name)
		}
	}
	m.mu.Unlock()
	sort.Strings(names)
	var gone []string
	for _, n := range names {
		if m.Remove(n, inUse) == nil {
			gone = append(gone, n)
		}
	}
	return gone
}

// AddDriver adds a host-provided network at run time (a class joined from
// a page); driverName describes it in `network ls`. RemoveDriver hands it
// back; Remove refuses it.
func (m *Manager) AddDriver(name, driverName string, d netdriver.NetworkDriver) (Network, error) {
	if !nameRE.MatchString(name) || name == Host || d == nil {
		return Network{}, fmt.Errorf("%w: driver name %q", ErrInvalid, name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.by[name] != nil {
		return Network{}, fmt.Errorf("%w: network with name %s already exists", ErrExists, name)
	}
	n := Network{Name: name, ID: idFor(name), Driver: driverName, Created: time.Now().UTC(), Builtin: true}
	m.by[name] = &entry{n, d}
	m.hosted = append(m.hosted, name)
	return n, nil
}

// RemoveDriver takes a network added by AddDriver away and returns its
// driver (the caller closes it): containers on it keep their sockets until
// they exit, and a later start of one fails with "network not found".
func (m *Manager) RemoveDriver(name string) (netdriver.NetworkDriver, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, h := range m.hosted {
		if h == name {
			e := m.by[name]
			delete(m.by, name)
			m.hosted = append(m.hosted[:i], m.hosted[i+1:]...)
			return e.drv, nil
		}
	}
	return nil, fmt.Errorf("%w: %s was not added at run time", ErrNotFound, name)
}

// Close closes every network.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.by {
		if e.drv != nil {
			e.drv.Close()
		}
	}
}
