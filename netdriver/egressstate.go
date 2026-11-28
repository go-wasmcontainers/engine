package netdriver

import "time"

// Egress policies a network can report (NetworkEgress.Policy).
const (
	// PolicyNone: nothing on this network can reach outside the engine.
	PolicyNone = "none"
	// PolicyInternal: the network is `internal: true`, and can never be given
	// egress.
	PolicyInternal = "internal"
	// PolicyGateway: the network's driver is a relay whose class has egress.
	PolicyGateway = "gateway"
	// PolicyUnknown: the driver says nothing, or says something this engine
	// does not know. Treated as "may leave" everywhere the engine decides.
	PolicyUnknown = "unknown"
)

// EgressPort is the only destination port a gateway carries (443/TCP). The
// engine does not display, and never advertises, any other.
const EgressPort = 443

// NetworkEgress is what the engine says, read-only, about a network's way out
// of the engine: for `network inspect` and for a UI that draws the gateway.
// It carries no secret and no payload.
type NetworkEgress struct {
	Policy    string         // PolicyNone | PolicyInternal | PolicyGateway | PolicyUnknown
	Reason    string         // "engine-internal network", "internal: true", "class relay", ...
	Gateway   *EgressGateway // nil unless Policy == PolicyGateway and the driver says who
	Allowlist []EgressDest   // what the operator allows, for display; 443/TCP only
	Limits    EgressLimits
	Counters  EgressCounters
}

// EgressGateway is the party that carries the traffic out.
type EgressGateway struct {
	Kind     string // "relay"
	Origin   string // https://host[:port]; never a token, a path or a query
	Operator string // label the operator set
	State    string // "up" | "down"
	Logs     bool   // destinations are logged by the operator
}

// EgressDest is one allowlisted destination: an exact host name, port 443.
type EgressDest struct {
	Host string
	Port uint16
}

// EgressLimits are the gateway's limits, as the relay reported them at join.
type EgressLimits struct {
	StreamsPerSession     uint32
	ConnectsPerSecond     float64
	ConnectBurst          uint32
	StreamsPerDestination uint32
	ClassBytesPerSecond   uint64
	SessionBytesPerDay    uint64
	IdleSeconds           uint32
	LifetimeSeconds       uint32
	LookupsPerSecond      float64
}

// EgressCounters are the gateway's counters as of the moment the driver
// learnt them (the relay's Welcome: at join, not live).
type EgressCounters struct {
	Allowed, DeniedNotAllowlisted, DeniedPrivateRange, DeniedTLSName,
	DeniedLimit, DroppedUDP uint64
	BytesUp, BytesDown uint64
	ActiveStreams      uint32
	LastDenied         *EgressDenial // no payload
}

// EgressDenial is the last thing the gateway refused.
type EgressDenial struct {
	Host   string
	Reason string
	Time   time.Time
}

// EgressReporter is an EgressDriver that also describes its egress, for
// display. EgressEnabled stays the authority for every decision; this is
// never consulted to allow anything.
type EgressReporter interface {
	EgressDriver
	EgressState() NetworkEgress
}
