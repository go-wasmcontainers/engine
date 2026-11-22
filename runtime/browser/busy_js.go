//go:build js

package browser

import (
	"runtime"
	"sync"
	"syscall/js"
)

var busy struct {
	once  sync.Once
	on    bool
	slot  js.Value // slot[0] = id marks the label of what runs now (busy.js)
	label js.Value
	ids   []any // label ids, boxed once: marking allocates nothing
	gcn   js.Value
	gcs   int32
}

// installBusy runs busy.js in this worker (once): from then on every slice of
// the engine's work is timed and the slow ones published as self.__busySlow.
func installBusy() {
	busy.once.Do(func() {
		g := js.Global()
		js.Global().Get("Function").New("globalThis", BusyJS).Invoke(g)
		b := g.Get("__busy")
		if !b.Truthy() {
			return // no Go glue to wrap (a test page): nothing is recorded
		}
		busy.slot, busy.label, busy.gcn = b.Get("slot"), b.Get("label"), b.Get("gcn")
		busy.ids = []any{0}
		busy.on = true
		armGC()
	})
}

// armGC counts the garbage collections: a finalizer runs once the sentinel
// is collected, counts it, and arms the next.
func armGC() {
	type sentinel struct{ _ [32]byte }
	s := new(sentinel)
	runtime.SetFinalizer(s, func(*sentinel) {
		busy.gcs++
		busy.gcn.SetIndex(0, int(busy.gcs))
		armGC()
	})
}

// Label registers a name for Mark and returns its id (0 is "unlabelled"; it
// is the id when nothing records). Call it once per site, not per use.
func Label(name string) int {
	installBusy()
	if !busy.on {
		return 0
	}
	id := busy.label.Invoke(name).Int()
	for len(busy.ids) <= id {
		busy.ids = append(busy.ids, len(busy.ids))
	}
	return id
}

// Mark says that what runs from now on in the engine worker's current slice
// is label id (Label's result; 0 unmarks). It allocates nothing: busy.js
// keeps the time under each label of a slice and publishes it with the slice
// if the slice is slow.
func Mark(id int) {
	if !busy.on {
		return
	}
	busy.slot.SetIndex(0, busy.ids[id])
}
