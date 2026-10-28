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
	} {
		if line := fmt.Sprintf("const %s = %d;", name, v); !strings.Contains(src, line) {
			t.Errorf("worker.js lacks %q", line)
		}
	}
	// The block is allocated on the Go side only; the worker must not
	// assume a size of its own.
	if strings.Contains(src, "new SharedArrayBuffer") {
		t.Error("worker.js allocates a control block")
	}
}
