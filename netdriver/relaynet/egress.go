package relaynet

import (
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-wasmcontainers/relay/egress"
	v1 "github.com/go-wasmcontainers/relay/gen/relayv1"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// maxAllowlist bounds the destinations shown: the relay is another party's
// machine, and what it sends is displayed.
const maxAllowlist = 256

// source is what the driver reads of its session: the relay's Welcome, and
// whether the session has ended. *driver.Session is one.
type source interface {
	Welcome() *v1.Welcome
	Done() <-chan struct{}
}

// egressBlock is the Welcome's egress block; nil when the relay sent none
// (a relay older than v0.4.0, which has no egress) or no Welcome is known.
func (d *Driver) egressBlock() (eg *v1.EgressState, known bool) {
	w := d.src.Welcome()
	if w == nil {
		return nil, false
	}
	return w.GetEgress(), true
}

// EgressEnabled implements netdriver.EgressDriver. It is false only when the
// relay positively said so: a Welcome with no egress block (a relay that has
// none) or Policy exactly "none". Anything else, a gateway, an empty or a
// policy this engine does not know, and no Welcome at all, is true: unknown
// fails closed, as an internal network requires.
func (d *Driver) EgressEnabled() bool {
	eg, known := d.egressBlock()
	if !known {
		return true
	}
	return eg != nil && eg.GetPolicy() != "none"
}

// NotifyEgress implements netdriver.EgressNotifier. The relay states its
// egress once, in the Welcome (v0.4.0 has no refresh message), so a
// session's EgressEnabled cannot turn true later and there is nothing to
// announce: fn is never called, and stop is a no-op.
func (d *Driver) NotifyEgress(fn func()) (stop func()) { return func() {} }

// SetOrigin records the relay's API URL for display, reduced to
// scheme://host[:port]: userinfo, path, query and fragment are dropped, so
// a token in the URL can never reach an inspect. An unusable URL leaves it
// empty.
func (d *Driver) SetOrigin(base string) {
	d.mu.Lock()
	d.origin = originOf(base)
	d.mu.Unlock()
}

// EgressState implements netdriver.EgressReporter: what the relay's Welcome
// said, for display. Counters are as of join, not live.
func (d *Driver) EgressState() netdriver.NetworkEgress {
	eg, known := d.egressBlock()
	switch {
	case !known:
		return netdriver.NetworkEgress{Policy: netdriver.PolicyUnknown, Reason: "the relay has not described its egress"}
	case eg == nil:
		return netdriver.NetworkEgress{Policy: netdriver.PolicyNone, Reason: "class relay without egress"}
	}
	switch eg.GetPolicy() {
	case "none":
		r := eg.GetReason()
		if r == "" {
			r = "class relay without egress"
		}
		return netdriver.NetworkEgress{Policy: netdriver.PolicyNone, Reason: r}
	case "gateway":
	default:
		return netdriver.NetworkEgress{Policy: netdriver.PolicyUnknown, Reason: "the relay reports a policy this engine does not know"}
	}
	d.mu.Lock()
	origin := d.origin
	d.mu.Unlock()
	state := "up"
	select {
	case <-d.src.Done():
		state = "down"
	default:
	}
	out := netdriver.NetworkEgress{
		Policy:    netdriver.PolicyGateway,
		Reason:    "class relay",
		Gateway:   &netdriver.EgressGateway{Kind: "relay", Origin: origin, Operator: eg.GetOperator(), State: state, Logs: eg.GetLogs()},
		Allowlist: destsOf(eg.GetAllowlist()),
	}
	if l := eg.GetLimits(); l != nil {
		out.Limits = netdriver.EgressLimits{
			StreamsPerSession: l.GetStreamsPerSession(), ConnectsPerSecond: l.GetConnectsPerSecond(), ConnectBurst: l.GetConnectBurst(),
			StreamsPerDestination: l.GetStreamsPerDestination(), ClassBytesPerSecond: l.GetClassBytesPerSecond(),
			SessionBytesPerDay: l.GetSessionBytesPerDay(), IdleSeconds: l.GetIdleSeconds(), LifetimeSeconds: l.GetLifetimeSeconds(),
			LookupsPerSecond: l.GetLookupsPerSecond(),
		}
	}
	if c := eg.GetCounters(); c != nil {
		out.Counters = netdriver.EgressCounters{
			Allowed: c.GetAllowed(), DeniedNotAllowlisted: c.GetDeniedNotAllowlisted(), DeniedPrivateRange: c.GetDeniedPrivateRange(),
			DeniedTLSName: c.GetDeniedTlsName(), DeniedLimit: c.GetDeniedLimit(), DroppedUDP: c.GetDroppedUdp(),
			BytesUp: c.GetBytesUp(), BytesDown: c.GetBytesDown(), ActiveStreams: c.GetActiveStreams(),
		}
		if h := c.GetLastDeniedHost(); h != "" {
			out.Counters.LastDenied = &netdriver.EgressDenial{Host: h, Reason: c.GetLastDeniedReason(), Time: time.Unix(c.GetLastDeniedUnix(), 0)}
		}
	}
	return out
}

// destsOf keeps the entries that are exact, lower-case, dotted host names on
// port 443 (the relay's own parser decides what that is), once each, at most
// maxAllowlist of them. Anything else the relay listed is not advertised:
// this engine shows 443/TCP to named hosts and nothing broader (no wildcard,
// no address, no other port).
func destsOf(entries []string) []netdriver.EgressDest {
	var out []netdriver.EgressDest
	seen := map[string]bool{}
	for _, e := range entries {
		if len(out) >= maxAllowlist {
			break
		}
		d, err := egress.ParseDest(e)
		if err != nil || d.Port != netdriver.EgressPort || seen[d.Host] {
			continue
		}
		seen[d.Host] = true
		out = append(out, netdriver.EgressDest{Host: d.Host, Port: d.Port})
	}
	return out
}

// originOf reduces an http(s) URL to scheme://host[:port], or "".
func originOf(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "https" && scheme != "http" {
		return ""
	}
	host := strings.ToLower(u.Host)
	if host == "" || u.Hostname() == "" {
		return ""
	}
	for i := 0; i < len(host); i++ {
		c := host[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '.' || c == '-' || c == ':' || c == '[' || c == ']') {
			return ""
		}
	}
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err != nil || n < 1 || n > 65535 {
			return ""
		}
	}
	return scheme + "://" + host
}
