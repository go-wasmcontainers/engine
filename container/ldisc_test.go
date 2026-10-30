package container

import (
	"bytes"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"

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
	l := newLdisc(term, pw, rawEcho{cr})
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

// A line editor switching to raw mode gets what was typed ahead in line
// mode: at once, and before the keys typed after the switch.
func TestLineModeOffHandsPendingOver(t *testing.T) {
	term := wasi.NewTerminal(80, 24, [3]bool{true, true, true})
	pr, pw := io.Pipe()
	l := newLdisc(term, pw, io.Discard)
	in, keys := io.Pipe()
	go l.run(in)
	keys.Write([]byte("l"))
	// The line is pending: nothing is readable yet.
	got := make(chan string, 1)
	go func() {
		b := make([]byte, 16)
		n, _ := pr.Read(b)
		got <- string(b[:n])
	}()
	select {
	case s := <-got:
		t.Fatalf("read %q before the switch", s)
	case <-time.After(50 * time.Millisecond):
	}
	term.SetMode(false, false) // the guest's tty_set; it must not block
	select {
	case s := <-got:
		if s != "l" {
			t.Fatalf("handed over %q", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pending line was not handed over")
	}
	keys.Write([]byte("s"))
	b := make([]byte, 16)
	if n, _ := pr.Read(b); string(b[:n]) != "s" {
		t.Fatalf("then %q", b[:n])
	}
	// Back in line mode, then off with nothing pending: nothing to hand.
	term.SetMode(true, true)
	term.SetMode(false, false)
	keys.Close()
	if rest, _ := io.ReadAll(pr); len(rest) != 0 {
		t.Fatalf("rest %q", rest)
	}
}

// A guest with a pending line that loops on tty_set(raw) and never reads
// stdin does not grow host goroutines: one hand-over runs at a time.
func TestLineModeOffNoGoroutineLeak(t *testing.T) {
	term := wasi.NewTerminal(80, 24, [3]bool{true, true, true})
	pr, pw := io.Pipe()
	defer pr.Close() // unblocks the stuck hand-over at the end
	l := newLdisc(term, pw, io.Discard)
	in, keys := io.Pipe()
	defer keys.Close()
	go l.run(in)
	keys.Write([]byte("pending")) // returns once the ldisc has the bytes
	for {
		l.mu.Lock()
		n := len(l.line)
		l.mu.Unlock()
		if n == len("pending") {
			break
		}
		time.Sleep(time.Millisecond)
	}
	settle := func() int {
		time.Sleep(50 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	before := settle()
	for range 10000 {
		term.SetMode(false, false)
	}
	if after := settle(); after > before+2 {
		t.Fatalf("goroutines %d -> %d after 10000 tty_set(raw)", before, after)
	}
}

// The pending line also goes first when the switch is seen by the next
// key rather than by the hand-over (whichever comes first).
func TestRawKeyAfterPending(t *testing.T) {
	term := wasi.NewTerminal(80, 24, [3]bool{true, true, true})
	pr, pw := io.Pipe()
	l := &ldisc{term: term, pw: pw, echo: io.Discard} // no hand-over hook
	go l.run(&switchReader{term: term, parts: []string{"ab", "c"}})
	if b, _ := io.ReadAll(pr); string(b) != "abc" {
		t.Errorf("%q", b)
	}
}

// switchReader returns parts, turning line mode off after the first.
type switchReader struct {
	term  *wasi.Terminal
	parts []string
	n     int
}

func (s *switchReader) Read(p []byte) (int, error) {
	if s.n == len(s.parts) {
		return 0, io.EOF
	}
	if s.n == 1 {
		s.term.SetMode(false, false)
	}
	s.n++
	return copy(p, s.parts[s.n-1]), nil
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
