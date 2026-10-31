package wasi

import (
	"encoding/binary"
	"testing"

	"github.com/go-wasmcontainers/engine/rootfs"
)

// isattyLibc is wasi-libc's __isatty (libc-bottom-half/sources/isatty.c):
// a character device whose base rights have neither FD_SEEK nor FD_TELL.
func isattyLibc(m mem) bool {
	const rightFdSeek, rightFdTell = 1 << 2, 1 << 5
	rights := binary.LittleEndian.Uint64(m[8:])
	return m[0] == ftCharDev && rights&(rightFdSeek|rightFdTell) == 0
}

func TestStdioFdstatPipeVsTTY(t *testing.T) {
	for _, tc := range []struct {
		name   string
		term   *Terminal
		tty    [3]bool
		rights [3]uint64
	}{
		{"no terminal (pipes)", nil, [3]bool{}, [3]uint64{rightFdRead, rightFdWrite, rightFdWrite}},
		{"terminal not attached", NewTerminal(80, 24, [3]bool{}), [3]bool{}, [3]uint64{rightFdRead, rightFdWrite, rightFdWrite}},
		{"all tty", NewTerminal(80, 24, [3]bool{true, true, true}), [3]bool{true, true, true}, [3]uint64{rightFdRead, rightFdWrite, rightFdWrite}},
		{"stdin pipe, output tty", NewTerminal(80, 24, [3]bool{false, true, true}), [3]bool{false, true, true}, [3]uint64{rightFdRead, rightFdWrite, rightFdWrite}},
		{"stdin tty, stdout pipe", NewTerminal(80, 24, [3]bool{true, false, true}), [3]bool{true, false, true}, [3]uint64{rightFdRead, rightFdWrite, rightFdWrite}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := New(Config{FS: rootfs.New(), Terminal: tc.term})
			m := make(mem, 64)
			for fd := uint64(0); fd < 3; fd++ {
				if r := call(t, p, m, "fd_fdstat_get", fd, 0); r.Errno != 0 {
					t.Fatal(fd, r)
				}
				want := uint8(ftUnknown)
				if tc.tty[fd] {
					want = ftCharDev
				}
				if m[0] != want {
					t.Errorf("fd %d filetype %d, want %d", fd, m[0], want)
				}
				if got := isattyLibc(m); got != tc.tty[fd] {
					t.Errorf("fd %d: wasi-libc isatty %v, want %v", fd, got, tc.tty[fd])
				}
				if b, i := binary.LittleEndian.Uint64(m[8:]), binary.LittleEndian.Uint64(m[16:]); b != tc.rights[fd] || i != tc.rights[fd] {
					t.Errorf("fd %d rights %#x/%#x, want %#x (wasmtime p1.rs)", fd, b, i, tc.rights[fd])
				}
				if r := call(t, p, m, "fd_filestat_get", fd, 0); r.Errno != 0 || m[16] != want {
					t.Errorf("fd %d filestat filetype %d, want %d", fd, m[16], want)
				}
			}
		})
	}
}
