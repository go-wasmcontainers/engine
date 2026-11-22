//go:build js

package browser

import (
	"runtime"
	"runtime/metrics"
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
	// gcstat is busy.js's Float64Array [GC CPU ms, cycles]; gcSample reads them.
	gcstat js.Value
}

var gcSample = [2]metrics.Sample{
	{Name: "/cpu/classes/gc/total:cpu-seconds"},
	{Name: "/gc/cycles/total:gc-cycles"},
}

// updateGC writes the collector's totals where busy.js reads them: its CPU time
// and the cycles it finished. Called after every host call and by busy.js at
// the end of a slow slice; the cost is one runtime/metrics read.
func updateGC() {
	if !busy.on || !busy.gcstat.Truthy() {
		return
	}
	metrics.Read(gcSample[:])
	if v := gcSample[0].Value; v.Kind() == metrics.KindFloat64 {
		busy.gcstat.SetIndex(0, v.Float64()*1000)
	}
	if v := gcSample[1].Value; v.Kind() == metrics.KindUint64 {
		busy.gcstat.SetIndex(1, float64(v.Uint64()))
	}
}

// installBusy picks up the busy log the worker loaded (busy.js, importScripts
// before Go started; never evaluated here, the CSP forbids it): every slice of
// the engine's work is timed and the slow ones published as self.__busySlow.
func installBusy() {
	busy.once.Do(func() {
		b := js.Global().Get("__busy")
		if !b.Truthy() {
			return // busy.js not loaded by the host page: nothing is recorded
		}
		busy.slot, busy.label, busy.gcn = b.Get("slot"), b.Get("label"), b.Get("gcn")
		busy.ids = []any{0}
		busy.on = true
		armGC()
		if st := b.Get("gcstat"); st.Truthy() {
			busy.gcstat = st
			b.Call("setReadGC", js.FuncOf(func(js.Value, []js.Value) any {
				updateGC()
				return nil
			}))
			updateGC()
		}
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
