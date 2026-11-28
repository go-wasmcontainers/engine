package relaynet

import (
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/go-wasmcontainers/relay/egress"
	v1 "github.com/go-wasmcontainers/relay/gen/relayv1"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// maxAllowlist bounds the destinations shown: the relay is another party's
// machine, and what it sends is displayed.
const maxAllowlist = 256

// Bounds on the free text and the clock the relay sends, which a UI draws.
const (
	maxOperator     = 64
	maxReason       = 128
	maxDenialReason = 64
	maxDenialHost   = 253
	// minDeniedUnix is 2000-01-01: no real denial is older. maxSkew is how far
	// ahead of this machine's clock a denial may claim to be.
	minDeniedUnix = 946684800
	maxSkew       = 24 * time.Hour
)

// nowFunc is the clock the denial time is judged against (a test replaces it).
var nowFunc = time.Now

// clean makes text from another party safe to draw: invalid UTF-8, control
// characters, format characters (which include every bidi override and
// isolate) and line and paragraph separators are dropped, and what is left is
// cut to max runes.
func clean(s string, max int) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.ToValidUTF8(s, "") {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r) {
			continue
		}
		if n == max {
			break
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// finite reports whether a rate the relay sent can be shown: a number, not
// infinite, not negative.
func finite(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 }

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
		r := clean(eg.GetReason(), maxReason)
		if r == "" {
			r = "class relay without egress"
		}
		return netdriver.NetworkEgress{Policy: netdriver.PolicyNone, Reason: r}
	case "gateway":
	default:
		return netdriver.NetworkEgress{Policy: netdriver.PolicyUnknown, Reason: "the relay reports a policy this engine does not know"}
	}
	if l := eg.GetLimits(); l != nil && !(finite(l.GetConnectsPerSecond()) && finite(l.GetLookupsPerSecond())) {
		// A rate that cannot be shown: the whole description is not believed
		// (EgressEnabled is untouched, so nothing is opened or hidden by it).
		return netdriver.NetworkEgress{Policy: netdriver.PolicyUnknown, Reason: "the relay reports limits this engine cannot show"}
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
		Gateway:   &netdriver.EgressGateway{Kind: "relay", Origin: origin, Operator: clean(eg.GetOperator(), maxOperator), State: state, Logs: eg.GetLogs()},
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
			AsOf: d.joined,
		}
		if h := clean(c.GetLastDeniedHost(), maxDenialHost); h != "" {
			den := &netdriver.EgressDenial{Host: h, Reason: clean(c.GetLastDeniedReason(), maxDenialReason)}
			// The relay's clock is not ours: a time outside [2000, now+1 day]
			// is not shown (zero), rather than drawn as a date.
			if u := c.GetLastDeniedUnix(); u >= minDeniedUnix && u <= nowFunc().Add(maxSkew).Unix() {
				den.Time = time.Unix(u, 0)
			}
			out.Counters.LastDenied = den
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
		// The port test is dead defence today: ParseDest already refuses
		// every port but 443. It stays so that a relay that widens its own
		// parser cannot make this engine advertise another port (D3).
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
