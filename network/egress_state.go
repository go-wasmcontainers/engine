package network

import (
	"slices"

	"github.com/go-wasmcontainers/engine/netdriver"
)

// Reasons a network reports for having no egress.
const (
	reasonEngineInternal = "engine-internal network"
	reasonInternalFlag   = "internal: true"
)

// view is the network as `ls` and `inspect` show it: the stored fields and
// the egress state, asked of the driver now.
func (e entry) view() Network {
	n := e.Network
	n.Egress = egressOf(n.Internal, e.drv)
	return n
}

// egressOf says what a network's way out is. It is a description, never a
// decision: attach and dial read netdriver.Confined, and so does this, so
// the report cannot say "none" about a driver the engine does not trust to
// be confined.
func egressOf(internal bool, d netdriver.NetworkDriver) netdriver.NetworkEgress {
	if internal {
		return netdriver.NetworkEgress{Policy: netdriver.PolicyInternal, Reason: reasonInternalFlag}
	}
	switch x := d.(type) {
	case nil, *netdriver.Local:
		// bridge, none and the networks created here (EN netdriver/local.go).
		return netdriver.NetworkEgress{Policy: netdriver.PolicyNone, Reason: reasonEngineInternal}
	case netdriver.EgressReporter:
		on := x.EgressEnabled()
		st := x.EgressState()
		switch {
		case !on:
			// Declares none: that is the answer, whatever the description says.
			if st.Policy == netdriver.PolicyNone {
				return copyEgress(st)
			}
			return netdriver.NetworkEgress{Policy: netdriver.PolicyNone, Reason: "the driver declares no egress"}
		case st.Policy == netdriver.PolicyGateway:
			return copyEgress(st)
		}
		// May leave, but the description says otherwise or says nothing.
		return netdriver.NetworkEgress{Policy: netdriver.PolicyUnknown, Reason: "the driver's description disagrees with its egress declaration"}
	case netdriver.EgressDriver:
		if !x.EgressEnabled() {
			return netdriver.NetworkEgress{Policy: netdriver.PolicyNone, Reason: "the driver declares no egress"}
		}
		return netdriver.NetworkEgress{Policy: netdriver.PolicyGateway, Reason: "the driver declares egress"}
	}
	return netdriver.NetworkEgress{Policy: netdriver.PolicyUnknown, Reason: "the driver does not declare its egress"}
}

// copyEgress detaches what the driver returned from the driver's own memory.
func copyEgress(s netdriver.NetworkEgress) netdriver.NetworkEgress {
	s.Allowlist = slices.Clone(s.Allowlist)
	if s.Gateway != nil {
		g := *s.Gateway
		s.Gateway = &g
	}
	if s.Counters.LastDenied != nil {
		d := *s.Counters.LastDenied
		s.Counters.LastDenied = &d
	}
	return s
}
