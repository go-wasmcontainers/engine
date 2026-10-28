// Package portmap publishes a container's ports on the host: `wctr run -p`,
// Docker's HostConfig.PortBindings and PublishAllPorts.
//
// A container has no host socket of its own: its listeners live on the
// engine's networks (netdriver). So a published port is a listener the
// daemon opens on the host -- through a Host, which is net.Listen natively
// and something else in a browser -- and every connection it accepts is
// dialed into the container through the container's own endpoint. Nothing
// is routed and no packet is rewritten; a connection is copied, both ways,
// like Docker's userland proxy.
//
// ⛔ One difference from Docker, on purpose: a binding that names no host
// address listens on 127.0.0.1, not on every address. A training lab's
// container should not become a service on the machine's network because
// somebody typed `-p 8080:80`; `-p 0.0.0.0:8080:80` says so when it is meant.
package portmap

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"strings"
)

// Key is a container port and its protocol: Docker's "80/tcp".
type Key struct {
	Port  uint16
	Proto string // "tcp" or "udp"
}

func (k Key) String() string { return strconv.Itoa(int(k.Port)) + "/" + k.Proto }

// ErrInvalid is a port, a binding or a specification that cannot be.
var ErrInvalid = errors.New("invalid port specification")

// ParseKey reads "80", "80/tcp" or "53/udp". sctp is refused: the engine's
// networks carry TCP and UDP only.
func ParseKey(s string) (Key, error) {
	port, proto, _ := strings.Cut(s, "/")
	proto = strings.ToLower(proto)
	if proto == "" {
		proto = "tcp"
	}
	if err := checkProto(proto); err != nil {
		return Key{}, err
	}
	p, err := parsePort(port)
	if err != nil {
		return Key{}, err
	}
	return Key{Port: p, Proto: proto}, nil
}

func checkProto(proto string) error {
	switch proto {
	case "tcp", "udp":
		return nil
	case "sctp":
		return fmt.Errorf("%w: sctp: this engine's networks carry tcp and udp only", ErrInvalid)
	}
	return fmt.Errorf("%w: invalid proto: %s", ErrInvalid, proto)
}

// parsePort reads a port from 1 to 65535.
func parsePort(s string) (uint16, error) {
	n, err := strconv.ParseUint(s, 10, 16)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("%w: invalid port %q (1 to 65535)", ErrInvalid, s)
	}
	return uint16(n), nil
}

// parseRange reads "8000" or "8000-8010".
func parseRange(s string) (lo, hi uint16, err error) {
	a, b, isRange := strings.Cut(s, "-")
	if lo, err = parsePort(a); err != nil {
		return 0, 0, err
	}
	if !isRange || a == b {
		return lo, lo, nil
	}
	if hi, err = parsePort(b); err != nil {
		return 0, 0, err
	}
	if hi < lo {
		return 0, 0, fmt.Errorf("%w: invalid port range %s", ErrInvalid, s)
	}
	return lo, hi, nil
}

// Binding is where a container port is published on the host.
type Binding struct {
	// HostIP is the address to listen on; the zero Addr is DefaultHostIP.
	HostIP netip.Addr
	// HostPort, HostPortEnd: a port, a range to take the first free one
	// from, or 0 for one the system picks.
	HostPort, HostPortEnd uint16
}

// DefaultHostIP is the address a binding that names none listens on.
var DefaultHostIP = netip.AddrFrom4([4]byte{127, 0, 0, 1})

// Addr is the host address the binding listens on.
func (b Binding) Addr() netip.Addr {
	if !b.HostIP.IsValid() {
		return DefaultHostIP
	}
	return b.HostIP
}

// String is the binding as Docker writes it: "127.0.0.1:8080",
// "127.0.0.1:8000-8010", "127.0.0.1:" for one the system picks.
func (b Binding) String() string {
	port := ""
	if b.HostPort != 0 {
		port = strconv.Itoa(int(b.HostPort))
		if b.HostPortEnd > b.HostPort {
			port += "-" + strconv.Itoa(int(b.HostPortEnd))
		}
	}
	return net.JoinHostPort(b.Addr().String(), port)
}

// Map is a container's bindings, by port. A port in it with no binding is
// exposed and not published (Docker's `"2377/tcp": null`).
type Map map[Key][]Binding

// Keys are the map's ports, in order.
func (m Map) Keys() []Key {
	ks := make([]Key, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Slice(ks, func(i, j int) bool {
		if ks[i].Port != ks[j].Port {
			return ks[i].Port < ks[j].Port
		}
		return ks[i].Proto < ks[j].Proto
	})
	return ks
}

// Bindings counts the map's bindings.
func (m Map) Bindings() int {
	n := 0
	for _, bs := range m {
		n += len(bs)
	}
	return n
}

// MaxBindings bounds one container's bindings: each is a host listener.
const MaxBindings = 1024

// ParseSpec reads one `-p` value, as Docker does
// (github.com/docker/go-connections/nat.ParsePortSpec):
//
//	containerPort[/proto]
//	hostPort:containerPort[/proto]
//	ip:hostPort:containerPort[/proto]     ip:  [::1] for IPv6
//	ip::containerPort[/proto]             a host port the system picks
//
// Ports may be ranges of the same length ("8000-8010:80-90"); a host range
// with a single container port is where to take a free one from.
func ParseSpec(spec string) (Map, error) {
	ip, hostPort, ctrPort := splitParts(spec)
	ctrPort, proto, _ := strings.Cut(ctrPort, "/")
	if ctrPort == "" {
		return nil, fmt.Errorf("%w: no port specified: %s<empty>", ErrInvalid, spec)
	}
	proto = strings.ToLower(proto)
	if proto == "" {
		proto = "tcp"
	}
	if err := checkProto(proto); err != nil {
		return nil, err
	}
	var addr netip.Addr
	if ip != "" {
		if strings.HasPrefix(ip, "[") {
			if !strings.HasSuffix(ip, "]") {
				return nil, fmt.Errorf("%w: invalid IP address %s", ErrInvalid, ip)
			}
			ip = ip[1 : len(ip)-1]
		}
		a, err := netip.ParseAddr(ip)
		if err != nil || a.Zone() != "" {
			return nil, fmt.Errorf("%w: invalid IP address: %s", ErrInvalid, ip)
		}
		addr = a.Unmap()
	}
	lo, hi, err := parseRange(ctrPort)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid containerPort: %s", ErrInvalid, ctrPort)
	}
	var hlo, hhi uint16
	if hostPort != "" {
		if hlo, hhi, err = parseRange(hostPort); err != nil {
			return nil, fmt.Errorf("%w: invalid hostPort: %s", ErrInvalid, hostPort)
		}
		if hi-lo != hhi-hlo && hi != lo {
			return nil, fmt.Errorf("%w: invalid ranges specified for container and host Ports: %s and %s", ErrInvalid, ctrPort, hostPort)
		}
	}
	count := int(hi) - int(lo) + 1
	if count > MaxBindings {
		return nil, fmt.Errorf("%w: %s publishes %d ports, at most %d", ErrInvalid, spec, count, MaxBindings)
	}
	m := Map{}
	for i := 0; i < count; i++ {
		b := Binding{HostIP: addr}
		switch {
		case hostPort == "":
		case count == 1:
			b.HostPort, b.HostPortEnd = hlo, hhi
		default:
			b.HostPort = hlo + uint16(i)
			b.HostPortEnd = b.HostPort
		}
		k := Key{Port: lo + uint16(i), Proto: proto}
		m[k] = append(m[k], b)
	}
	return m, nil
}

// splitParts splits ip:hostPort:containerPort; the ip may itself hold
// colons (an IPv6 address in brackets), so it is everything before the
// last two.
func splitParts(s string) (ip, hostPort, ctrPort string) {
	parts := strings.Split(s, ":")
	switch n := len(parts); n {
	case 1:
		return "", "", parts[0]
	case 2:
		return "", parts[0], parts[1]
	default:
		return strings.Join(parts[:n-2], ":"), parts[n-2], parts[n-1]
	}
}

// ParseSpecs reads every `-p` value into one map.
func ParseSpecs(specs []string) (Map, error) {
	out := Map{}
	for _, s := range specs {
		m, err := ParseSpec(s)
		if err != nil {
			return nil, err
		}
		out.Merge(m)
		if out.Bindings() > MaxBindings {
			return nil, fmt.Errorf("%w: at most %d published ports", ErrInvalid, MaxBindings)
		}
	}
	return out, nil
}

// Merge adds o's bindings to m.
func (m Map) Merge(o Map) {
	for k, bs := range o {
		if _, ok := m[k]; !ok {
			m[k] = nil
		}
		m[k] = append(m[k], bs...)
	}
}

// ParseBinding reads the API's PortBinding: a HostIp ("" for the default)
// and a HostPort ("", "8080", "8000-8010").
func ParseBinding(hostIP, hostPort string) (Binding, error) {
	var b Binding
	if hostIP != "" {
		a, err := netip.ParseAddr(strings.TrimSuffix(strings.TrimPrefix(hostIP, "["), "]"))
		if err != nil || a.Zone() != "" {
			return b, fmt.Errorf("%w: invalid IP address: %s", ErrInvalid, hostIP)
		}
		b.HostIP = a.Unmap()
	}
	if hostPort != "" {
		lo, hi, err := parseRange(hostPort)
		if err != nil {
			return b, fmt.Errorf("%w: invalid hostPort: %s", ErrInvalid, hostPort)
		}
		b.HostPort, b.HostPortEnd = lo, hi
	}
	return b, nil
}
