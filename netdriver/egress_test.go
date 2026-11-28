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

type hidden struct{ NetworkDriver }

// Confined is a positive declaration: a *Local, or an EgressDriver that says
// false now. A driver that says nothing, and a wrapper hiding the method, are
// not confined.
func TestConfined(t *testing.T) {
	l, err := NewLocal(DefaultPrefix)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	for _, tc := range []struct {
		name string
		d    NetworkDriver
		want bool
	}{
		{"nil", nil, false},
		{"local", l, true},
		{"declared off", egressStub{l, false}, true},
		{"declared on", egressStub{l, true}, false},
		{"wrapped off", hidden{egressStub{l, false}}, false},
		{"wrapped local", hidden{l}, false},
	} {
		if got := Confined(tc.d); got != tc.want {
			t.Errorf("%s: Confined = %v, want %v", tc.name, got, tc.want)
		}
	}
}
