package netdriver

import "testing"

type egressStub struct {
	*Local
	on bool
}

func (e egressStub) EgressEnabled() bool { return e.on }

func TestHasEgress(t *testing.T) {
	l, err := NewLocal(DefaultPrefix)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if HasEgress(l) {
		t.Error("Local has egress")
	}
	if HasEgress(nil) {
		t.Error("nil has egress")
	}
	if HasEgress(egressStub{l, false}) {
		t.Error("a driver with egress off has egress")
	}
	if !HasEgress(egressStub{l, true}) {
		t.Error("a driver with egress on has none")
	}
}
