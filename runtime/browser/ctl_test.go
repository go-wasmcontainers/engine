package browser

import (
	"fmt"
	"strings"
	"testing"
)

// TestWorkerMirrorsControlBlock: worker.js declares the control block's
// layout as constants; each must equal ctl.go's, or the two sides would
// read each other's fields at the wrong places.
func TestWorkerMirrorsControlBlock(t *testing.T) {
	src := string(WorkerJS)
	for name, v := range map[string]int{
		"CTL_STATE": ctlState, "CTL_FUNC": ctlFunc, "CTL_ERRNO": ctlErrno, "CTL_FLAGS": ctlFlags,
		"CTL_SLEEP": ctlSleep, "CTL_CODE": ctlCode, "CTL_TARGET": ctlTarget, "CTL_ALIGN": ctlAlign,
		"CTL_SIZE_ARG": ctlSizeArg, "CTL_PTR": ctlPtr, "CTL_ARGS": ctlArgs, "CTL_RET": ctlRet,
		"STATE_DONE": stateDone, "FLAG_EXIT": flagExit, "FLAG_REALLOC": flagRealloc,
		"CTL_SENT": ctlSent, "CTL_SERVED": ctlServed, "CTL_ANSWER": ctlAnswer,
		"SLOW_CALL": slowCall, "SLOW_CAP": slowCap,
	} {
		if line := fmt.Sprintf("const %s = %d;", name, v); !strings.Contains(src, line) {
			t.Errorf("worker.js lacks %q", line)
		}
	}
	// The three timestamps are float64 slots on 8-byte boundaries that fit the
	// block, after the result.
	for name, off := range map[string]int{"sent": ctlSent, "served": ctlServed, "answer": ctlAnswer} {
		if off%8 != 0 || off < ctlRet+8 || off+8 > ctlSize {
			t.Errorf("the %s timestamp at byte %d does not fit after the result in %d bytes", name, off, ctlSize)
		}
	}
	// A call is split where the guest woke: the wait for the engine's thread,
	// its execution, the return trip, from the same three stamps and the wake.
	for _, want := range []string{
		"wait: served - sent + d", "exec: answered - served", "ret: woke - answered - d",
		`postMessage({ t: "slow"`, "performance.timeOrigin + performance.now()",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("worker.js lacks %q", want)
		}
	}
	// The block is allocated on the Go side only; the worker must not
	// assume a size of its own.
	if strings.Contains(src, "new SharedArrayBuffer") {
		t.Error("worker.js allocates a control block")
	}
}
