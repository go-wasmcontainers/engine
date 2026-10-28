//go:build wasip1

// Command wasixguest calls the engine's WASIX host module ("wasix_32v1")
// the way a wasix-libc program would: proc_spawn2/3 with fd ops, proc_join,
// fd_pipe, proc_id/proc_parent, tty_get. It re-runs itself as the child;
// the first argument picks the case (runtime/wazero wasix_test.go).
//
// Its bindings are written here from wasix-libc's api_wasix.h, apart from
// any other client, so the tests check the host against the header.
package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

//go:wasmimport wasix_32v1 proc_spawn2
func procSpawn2(name unsafe.Pointer, nameLen uint32, args unsafe.Pointer, argsLen uint32, envs unsafe.Pointer, envsLen uint32,
	fdOps unsafe.Pointer, fdOpsLen uint32, sig unsafe.Pointer, sigLen uint32, search uint32, path unsafe.Pointer, pathLen uint32, ret unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 proc_spawn3
func procSpawn3(name unsafe.Pointer, nameLen uint32, args unsafe.Pointer, argsLen uint32, envs unsafe.Pointer, envsLen uint32,
	fdOps unsafe.Pointer, fdOpsLen uint32, sig unsafe.Pointer, sigLen uint32, search uint32, path unsafe.Pointer, pathLen uint32, ret unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 proc_join
func procJoin(pid unsafe.Pointer, flags uint32, status unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 fd_pipe
func fdPipe(r, w unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 proc_id
func procID(ret unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 proc_parent
func procParent(pid uint32, ret unsafe.Pointer) uint32

//go:wasmimport wasix_32v1 tty_get
func ttyGet(ret unsafe.Pointer) uint32

const self = "/bin/wasixguest.wasm"

func addr(p unsafe.Pointer) uint32 { return uint32(uintptr(p)) }

type fdop struct {
	cmd     byte
	fd, src uint32
	path    string
}

// spawn3 starts name with argv (C strings), env (nil inherits) and ops.
func spawn3(name string, argv, env []string, ops []fdop, search bool, path string) (uint32, uint32) {
	keep := [][]byte{}
	cs := func(list []string) []uint32 {
		out := make([]uint32, len(list))
		for i, s := range list {
			b := append([]byte(s), 0)
			keep = append(keep, b)
			out[i] = addr(unsafe.Pointer(&b[0]))
		}
		return out
	}
	av := cs(argv)
	var envPtr unsafe.Pointer
	var envLen uint32
	if env != nil {
		ev := cs(env)
		ev = append(ev, 0) // never empty, so the pointer is not null
		envPtr, envLen = unsafe.Pointer(&ev[0]), uint32(len(env))
		defer runtime.KeepAlive(ev)
	}
	opb, opPtr := encodeOps(ops)
	nb := []byte(name)
	pb := append([]byte(path), 0)
	var s uint32
	if search {
		s = 1
	}
	var pid uint32
	var avPtr unsafe.Pointer
	if len(av) > 0 {
		avPtr = unsafe.Pointer(&av[0])
	}
	e := procSpawn3(unsafe.Pointer(&nb[0]), uint32(len(nb)), avPtr, uint32(len(av)), envPtr, envLen,
		opPtr, uint32(len(ops)), nil, 0, s, unsafe.Pointer(&pb[0]), uint32(len(path)), unsafe.Pointer(&pid))
	runtime.KeepAlive(keep)
	runtime.KeepAlive(opb)
	return pid, e
}

// encodeOps lays out __wasi_proc_spawn_fd_op_t (56 bytes, align 8).
func encodeOps(ops []fdop) ([]uint64, unsafe.Pointer) {
	if len(ops) == 0 {
		return nil, nil
	}
	words := make([]uint64, 7*len(ops))
	b := unsafe.Slice((*byte)(unsafe.Pointer(&words[0])), 8*len(words))
	paths := [][]byte{}
	for i, op := range ops {
		r := b[56*i:]
		r[0] = op.cmd
		binary.LittleEndian.PutUint32(r[4:], op.fd)
		binary.LittleEndian.PutUint32(r[8:], op.src)
		if op.path != "" {
			pb := []byte(op.path)
			paths = append(paths, pb)
			binary.LittleEndian.PutUint32(r[12:], addr(unsafe.Pointer(&pb[0])))
			binary.LittleEndian.PutUint32(r[16:], uint32(len(pb)))
		}
		if op.cmd == 2 { // open: write rights, creat|trunc
			binary.LittleEndian.PutUint16(r[24:], 1|8)
			binary.LittleEndian.PutUint64(r[32:], 1<<6)
		}
	}
	runtime.KeepAlive(paths)
	return words, unsafe.Pointer(&words[0])
}

// join waits for pid (0: any); it returns the pid, tag, code, signal.
func join(pid uint32, nonblock bool) (uint32, byte, uint16, byte, uint32) {
	var opt [2]uint32
	if pid != 0 {
		opt[0], opt[1] = 1, pid
	}
	var st [4]uint16 // 6 bytes used, aligned to 2
	var flags uint32
	if nonblock {
		flags = 1
	}
	e := procJoin(unsafe.Pointer(&opt[0]), flags, unsafe.Pointer(&st[0]))
	sb := unsafe.Slice((*byte)(unsafe.Pointer(&st[0])), 6)
	return opt[1], sb[0], binary.LittleEndian.Uint16(sb[2:]), sb[4], e
}

func pipe() (uint32, uint32) {
	var r, w uint32
	if e := fdPipe(unsafe.Pointer(&r), unsafe.Pointer(&w)); e != 0 {
		fail("fd_pipe: %d", e)
	}
	return r, w
}

func fail(f string, a ...any) {
	fmt.Fprintf(os.Stderr, f+"\n", a...)
	os.Exit(2)
}

// out runs a child with its stdout on a pipe and returns what it wrote.
func out(argv []string, env []string, extra ...fdop) (string, uint16, byte) {
	r, w := pipe()
	ops := append(extra, fdop{cmd: 1, fd: 1, src: w}, fdop{cmd: 0, fd: w}, fdop{cmd: 0, fd: r})
	pid, e := spawn3(self, argv, env, ops, false, "")
	if e != 0 {
		fail("spawn %v: %d", argv, e)
	}
	syscall.Close(int(w))
	b, _ := io.ReadAll(os.NewFile(uintptr(r), "pipe"))
	_, tag, code, sig, je := join(pid, false)
	if je != 0 || tag == 0 {
		fail("join: %d tag %d", je, tag)
	}
	return string(b), code, sig
}

func main() {
	if len(os.Args) < 2 {
		fail("usage: wasixguest CASE")
	}
	switch os.Args[1] {
	// Children.
	case "child-echo":
		fmt.Printf("%q\n", os.Args[2:])
	case "child-exit":
		n, _ := strconv.Atoi(os.Args[2])
		os.Exit(n)
	case "child-cat": // stdin as Go's runtime leaves it (non-blocking, polled)
		io.Copy(os.Stdout, os.Stdin)
	case "child-fstat":
		var st syscall.Stat_t
		if err := syscall.Fstat(0, &st); err != nil {
			fmt.Println("fstat:", err)
			os.Exit(1)
		}
		fmt.Println("fstat ok")
	case "child-pwd":
		wd, _ := os.Getwd()
		fmt.Println(wd)
	case "child-ids":
		var me, pp uint32
		procID(unsafe.Pointer(&me))
		procParent(me, unsafe.Pointer(&pp))
		fmt.Println(me, pp)

	// Cases.
	case "argv": // empty and newline arguments survive proc_spawn3
		s, code, _ := out([]string{"wasixguest", "child-echo", "", "a\nb", "c"}, nil)
		fmt.Print(s, code, "\n")
	case "spawn2": // proc_spawn2: line-feed separated, empty entries dropped
		r, w := pipe()
		name := []byte(self)
		args := []byte("wasixguest\nchild-echo\nx\n\ny\n")
		ops, opPtr := encodeOps([]fdop{{cmd: 1, fd: 1, src: w}, {cmd: 0, fd: w}, {cmd: 0, fd: r}})
		var pid uint32
		e := procSpawn2(unsafe.Pointer(&name[0]), uint32(len(name)), unsafe.Pointer(&args[0]), uint32(len(args)), nil, 0,
			opPtr, 3, nil, 0, 0, nil, 0, unsafe.Pointer(&pid))
		runtime.KeepAlive(ops)
		if e != 0 {
			fail("proc_spawn2: %d", e)
		}
		syscall.Close(int(w))
		b, _ := io.ReadAll(os.NewFile(uintptr(r), "pipe"))
		join(pid, false)
		fmt.Print(string(b))
	case "codes": // exit codes travel raw (wasmer turns those above 80 into 79)
		var got []string
		for _, c := range []string{"0", "3", "78", "79", "255", "300"} {
			pid, e := spawn3(self, []string{"wasixguest", "child-exit", c}, nil, nil, false, "")
			if e != 0 {
				fail("spawn: %d", e)
			}
			_, tag, code, _, _ := join(pid, false)
			got = append(got, fmt.Sprintf("%d:%d", tag, code))
		}
		fmt.Println(strings.Join(got, " "))
	case "latestdin": // a Go child reading a pipe that fills late (wasmer: EAGAIN)
		r, w := pipe()
		pid, e := spawn3(self, []string{"wasixguest", "child-cat"}, nil,
			[]fdop{{cmd: 1, fd: 0, src: r}, {cmd: 0, fd: r}, {cmd: 0, fd: w}}, false, "")
		if e != 0 {
			fail("spawn: %d", e)
		}
		syscall.Close(int(r))
		time.Sleep(200 * time.Millisecond)
		f := os.NewFile(uintptr(w), "pipe")
		f.WriteString("late\n")
		f.Close()
		_, _, code, _, _ := join(pid, false)
		fmt.Println("code", code)
	case "fstat": // a pipe end has a filestat (wasmer: refused)
		r, w := pipe()
		s, code, _ := out([]string{"wasixguest", "child-fstat"}, nil,
			fdop{cmd: 1, fd: 0, src: r}, fdop{cmd: 0, fd: r}, fdop{cmd: 0, fd: w})
		syscall.Close(int(w))
		fmt.Print(s, code, "\n")
	case "readerfirst": // a goroutine parked on an empty pipe (wasmer: freezes)
		r, w := pipe()
		syscall.SetNonblock(int(r), true)
		syscall.SetNonblock(int(w), true)
		rf, wf := os.NewFile(uintptr(r), "r"), os.NewFile(uintptr(w), "w")
		done := make(chan int)
		go func() {
			b, _ := io.ReadAll(rf)
			done <- strings.Count(string(b), "\n")
		}()
		time.Sleep(50 * time.Millisecond)
		go func() {
			for i := 0; i < 20000; i++ {
				fmt.Fprintf(wf, "line %d\n", i)
			}
			wf.Close()
		}()
		select {
		case n := <-done:
			fmt.Println("lines", n)
		case <-time.After(20 * time.Second):
			fail("stalled")
		}
	case "joinany":
		for _, c := range []string{"1", "2"} {
			if _, e := spawn3(self, []string{"wasixguest", "child-exit", c}, nil, nil, false, ""); e != 0 {
				fail("spawn: %d", e)
			}
		}
		var codes []int
		for range 2 {
			_, _, code, _, e := join(0, false)
			if e != 0 {
				fail("join any: %d", e)
			}
			codes = append(codes, int(code))
		}
		sort.Ints(codes)
		_, _, _, _, e := join(0, false)
		fmt.Println(codes, "then", e)
	case "nonblock":
		r, w := pipe()
		pid, _ := spawn3(self, []string{"wasixguest", "child-cat"}, nil,
			[]fdop{{cmd: 1, fd: 0, src: r}, {cmd: 0, fd: r}, {cmd: 0, fd: w}}, false, "")
		syscall.Close(int(r))
		_, tag, _, _, e := join(pid, true)
		fmt.Println("running", tag, e)
		syscall.Close(int(w))
		_, tag, code, _, e := join(pid, false)
		fmt.Println("ended", tag, code, e)
	case "errors":
		_, e1 := spawn3("/bin/nope", []string{"x"}, nil, nil, false, "")
		_, e2 := spawn3("/etc/motd", []string{"x"}, nil, nil, false, "")
		_, e3 := spawn3("nope", []string{"x"}, nil, nil, true, "/bin")
		_, e4 := spawn3(self, []string{"x"}, []string{"NOEQUALS"}, nil, false, "")
		_, e5 := spawn3(self, []string{"x"}, nil, []fdop{{cmd: 9}}, false, "")
		_, e6 := spawn3(self, []string{"x"}, nil, []fdop{{cmd: 0, fd: 77}}, false, "")
		fmt.Println(e1, e2, e3, e4, e5, e6)
	case "search": // PATH lookup tries <dir>/<name> then <dir>/<name>.wasm
		r, w := pipe()
		pid, e := spawn3("wasixguest", []string{"wasixguest", "child-echo", "found"}, nil,
			[]fdop{{cmd: 1, fd: 1, src: w}, {cmd: 0, fd: w}, {cmd: 0, fd: r}}, true, "/nowhere:/bin")
		if e != 0 {
			fail("search: %d", e)
		}
		syscall.Close(int(w))
		b, _ := io.ReadAll(os.NewFile(uintptr(r), "pipe"))
		join(pid, false)
		fmt.Print(string(b))
	case "chdir": // CHDIR op, and $PWD for an inherited environment
		s, _, _ := out([]string{"wasixguest", "child-pwd"}, nil, fdop{cmd: 3, path: "/etc"})
		fmt.Print(s)
	case "ids":
		s, _, _ := out([]string{"wasixguest", "child-ids"}, nil)
		var me uint32
		procID(unsafe.Pointer(&me))
		fmt.Print("parent ", me, " child ", s)
	case "redirect": // OPEN op: the child's stdout to a new file
		pid, e := spawn3(self, []string{"wasixguest", "child-echo", "in a file"}, nil,
			[]fdop{{cmd: 2, fd: 1, path: "/out"}}, false, "")
		if e != 0 {
			fail("spawn: %d", e)
		}
		join(pid, false)
		b, err := os.ReadFile("/out")
		fmt.Print(string(b), err, "\n")
	case "tty":
		var b [6]uint32
		e := ttyGet(unsafe.Pointer(&b[0]))
		bb := unsafe.Slice((*byte)(unsafe.Pointer(&b[0])), 24)
		fmt.Println(e, b[0], b[1], bb[16], bb[17], bb[18], bb[19], bb[20])
	case "bomb": // spawn until the group refuses
		n := 0
		for {
			r, w := pipe()
			_, e := spawn3(self, []string{"wasixguest", "child-cat"}, nil,
				[]fdop{{cmd: 1, fd: 0, src: r}, {cmd: 0, fd: r}, {cmd: 0, fd: w}}, false, "")
			syscall.Close(int(r))
			if e != 0 {
				fmt.Println("refused after", n, "with", e)
				return
			}
			n++
		}
	default:
		fail("unknown case %q", os.Args[1])
	}
}
