package container

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/wasi"
)

// discipline runs input through a line discipline and returns what the
// process read and what was echoed.
func discipline(t testing.TB, echo, line bool, input string) (string, string) {
	term := wasi.NewTerminal(80, 24, [3]bool{true, true, true})
	term.SetMode(echo, line)
	var shown bytes.Buffer
	cr := &crlf{w: &shown}
	pr, pw := io.Pipe()
	l := &ldisc{term: term, pw: pw, echo: rawEcho{cr}}
	go l.run(strings.NewReader(input))
	got, _ := io.ReadAll(pr)
	return string(got), shown.String()
}

func TestLineDiscipline(t *testing.T) {
	cases := []struct {
		name, in   string
		echo, line bool
		got, shown string
	}{
		{"line, echo", "ls\r", true, true, "ls\n", "ls\r\n"},
		{"erase", "lx\x7fs\n", true, true, "ls\n", "lx\b \bs\r\n"},
		{"backspace on empty", "\x08a\n", true, true, "a\n", "a\r\n"},
		{"kill line", "abc\x15d\n", true, true, "d\n", "abc\b \b\b \b\b \bd\r\n"},
		{"^C drops the line", "abc\x03d\n", true, true, "d\n", "abc^C\r\nd\r\n"},
		{"^D ends input on an empty line", "a\n\x04b\n", true, true, "a\n", "a\r\n"},
		{"^D sends a partial line", "ab\x04", false, true, "ab", ""},
		{"no echo", "secret\n", false, true, "secret\n", ""},
		{"raw", "a\x7f\x03\r", false, false, "a\x7f\x03\r", ""},
		{"raw with echo", "ab", true, false, "ab", "ab"},
		{"end of input flushes", "partial", false, true, "partial", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, shown := discipline(t, c.echo, c.line, c.in)
			if got != c.got || shown != c.shown {
				t.Errorf("read %q echo %q, want %q and %q", got, shown, c.got, c.shown)
			}
		})
	}
	long, _ := discipline(t, false, true, strings.Repeat("x", MaxLine+100)+"\n")
	if len(long) != MaxLine+1 {
		t.Errorf("an over-long line gave %d bytes", len(long))
	}
}

func TestCRLF(t *testing.T) {
	var b bytes.Buffer
	c := &crlf{w: &b}
	c.Write([]byte("a\nb\r\nc"))
	c.Write([]byte("\n"))
	c.Write([]byte("plain"))
	if b.String() != "a\r\nb\r\nc\r\nplain" {
		t.Errorf("%q", b.String())
	}
}

// FuzzLineDiscipline: any input, any mode — what the process reads is
// bounded by what was typed, and nothing hangs or panics.
func FuzzLineDiscipline(f *testing.F) {
	f.Add([]byte("ls -l\r\x7f\x03\x04"), true, true)
	f.Fuzz(func(t *testing.T, in []byte, echo, line bool) {
		got, _ := discipline(t, echo, line, string(in))
		if len(got) > len(in) {
			t.Fatalf("read %d bytes from %d typed", len(got), len(in))
		}
	})
}
