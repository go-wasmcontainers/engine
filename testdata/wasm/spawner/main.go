//go:build wasip1

// Command spawner exercises the engine's process primitives through the
// test host module "wctr_proc" (runtime/wazero proc_test.go): pipes,
// spawning a child with its stdout on a pipe, joining it, and the errors
// of a spawn. The first argument picks the case.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unsafe"
)

//go:wasmimport wctr_proc pipe
func pipe(ret unsafe.Pointer) uint32

//go:wasmimport wctr_proc spawn
func spawn(path unsafe.Pointer, pathLen uint32, argv unsafe.Pointer, argvLen uint32, outFD uint32, pid unsafe.Pointer) uint32

//go:wasmimport wctr_proc join
func join(pid uint32, status unsafe.Pointer) uint32

//go:wasmimport wctr_proc getpid
func getpid() uint32

func start(path string, out uint32, argv ...string) (uint32, uint32) {
	a := strings.Join(argv, "\x00")
	p := []byte(path)
	ab := []byte(a + "\x00")
	var pid uint32
	e := spawn(unsafe.Pointer(&p[0]), uint32(len(p)), unsafe.Pointer(&ab[0]), uint32(len(ab)), out, unsafe.Pointer(&pid))
	return pid, e
}

func wait(pid uint32) (uint32, uint32, uint32) {
	var st [2]uint32
	e := join(pid, unsafe.Pointer(&st))
	return st[0], st[1], e
}

func main() {
	switch os.Args[1] {
	case "pipeline":
		var fds [2]uint32
		if e := pipe(unsafe.Pointer(&fds)); e != 0 {
			fmt.Println("pipe errno", e)
			return
		}
		pid, e := start("/bin/hello.wasm", fds[1], "hello")
		if e != 0 {
			fmt.Println("spawn errno", e)
			return
		}
		w := os.NewFile(uintptr(fds[1]), "w")
		w.Close() // only the child holds the write end now
		r := os.NewFile(uintptr(fds[0]), "r")
		b, err := io.ReadAll(r)
		code, sig, je := wait(pid)
		fmt.Printf("me=%d child=%d read=%q err=%v code=%d sig=%d join=%d\n", getpid(), pid, b, err, code, sig, je)
	case "codes":
		pid, _ := start("/bin/probe.wasm", 1, "probe", "exit", "300")
		code, sig, e := wait(pid)
		fmt.Println(code, sig, e)
		_, e1 := start("/bin/missing.wasm", 1, "x")
		_, e2 := start("/etc/motd", 1, "x")
		_, _, e3 := wait(0) // no children left
		fmt.Println(e1, e2, e3)
	case "orphan":
		// A child that never ends: the container must still stop when
		// this, its first process, exits.
		start("/bin/probe.wasm", 1, "probe", "spin")
		fmt.Println("leaving")
	}
}
