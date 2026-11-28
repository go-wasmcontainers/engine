package network

import (
	"errors"
	"syscall"
	"testing"
)

// The notice is permanent for the NETWORK, not for the endpoint that was
// attached when it came: a container that attaches after the one-shot notice,
// before EgressEnabled reads true, is refused (it is born tripped).
func TestSealedNoticeStaysForLaterEndpoints(t *testing.T) {
	ctx := ctx5(t)
	d := notifyDriver{localDriver(t), &notifyState{}}
	m, _, _ := New(Options{})
	defer m.Close()
	if _, err := m.AddDriverWith("late", "relay", d, true); err != nil {
		t.Fatal(err)
	}
	a, err := m.Attach(ctx, "late", "a", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Detach()
	// The notice only: EgressEnabled still reads false.
	d.st.mu.Lock()
	var fns []func()
	for _, f := range d.st.fns {
		fns = append(fns, f)
	}
	d.st.mu.Unlock()
	for _, f := range fns {
		f()
	}
	b, err := m.Attach(ctx, "late", "b", nil)
	if err == nil {
		b.Detach()
		t.Fatal("an endpoint attached after the notice was admitted")
	}
	if !errors.Is(err, ErrEgress) || !errors.Is(err, syscall.ENETUNREACH) {
		t.Fatalf("err = %v, want ErrEgress and ENETUNREACH", err)
	}
}
