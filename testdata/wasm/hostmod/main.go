//go:build wasip1

// Command hostmod exercises a host module (runtime/wazero's test module
// "wctr_test"): typed parameters and results (i32, i64, f32, f64), a
// result the host allocates with cabi_realloc, a call that waits, and a
// call that traps. The first argument picks the case; it prints what it
// got.
package main

import (
	"fmt"
	"os"
	"unsafe"
)

var pinned [][]byte

//go:wasmexport cabi_realloc
func cabiRealloc(old, oldSize, align, size uint32) uint32 {
	b := make([]byte, size+align)
	pinned = append(pinned, b)
	p := uintptr(unsafe.Pointer(&b[0]))
	p = (p + uintptr(align) - 1) &^ (uintptr(align) - 1)
	return uint32(p)
}

//go:wasmimport wctr_test add
func add(a, b uint32) uint32

//go:wasmimport wctr_test mix
func mix(a int64, b float32, c float64) float64

//go:wasmimport wctr_test neg
func neg(a float32) float32

//go:wasmimport wctr_test wide
func wide(a int64) int64

//go:wasmimport wctr_test hello
func hello(ret unsafe.Pointer)

//go:wasmimport wctr_test later
func later() uint32

//go:wasmimport wctr_test boom
func boom()

//go:wasmimport wctr_test cap
func capGPU() uint32

func main() {
	switch os.Args[1] {
	case "types":
		fmt.Println(add(40, 2), mix(-3, 0.5, 0.25), neg(1.5), wide(-1<<40))
	case "realloc":
		var l [2]uint32
		hello(unsafe.Pointer(&l))
		fmt.Println(string(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(l[0]))), l[1])))
	case "wait":
		fmt.Println(later())
	case "trap":
		boom()
		fmt.Println("not reached")
	case "cap":
		fmt.Println(capGPU())
	}
}
