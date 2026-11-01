package lineedit

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

// flaky returns nothing on every other Read, as a terminal may.
type flaky struct {
	r    io.Reader
	flip bool
}

func (f *flaky) Read(p []byte) (int, error) {
	f.flip = !f.flip
	if f.flip {
		return 0, nil
	}
	return f.r.Read(p)
}

// failing delivers data, then err instead of io.EOF.
type failing struct {
	data string
	err  error
}

func (f *failing) Read(p []byte) (int, error) {
	if f.data == "" {
		return 0, f.err
	}
	p[0], f.data = f.data[0], f.data[1:]
	return 1, nil
}

func newReader(in string) (*Reader, *bytes.Buffer) {
	out := &bytes.Buffer{}
	return &Reader{In: strings.NewReader(in), Out: out}, out
}

func readOne(t *testing.T, in string) (string, error, string) {
	t.Helper()
	r, out := newReader(in)
	l, err := r.ReadLine(context.Background(), "$ ")
	return l, err, out.String()
}

func TestReadLineEditing(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"ls\r", "ls"},
		{"ls\n", "ls"},
		{"abc\x1b[D\x1b[DX\r", "aXbc"},
		{"ab\x1bODX\r", "aXb"},
		{"ab\x1b[1;5DX\r", "aXb"},
		{"a\x1bxb\r", "ab"},     // Alt-key: ignored
		{"a\x1b[\x01b\r", "ab"}, // not a CSI sequence after all
		{"a\x1b[?25hb\r", "ab"}, // a private CSI sequence is no text
		{"a\x1b[1 qb\r", "ab"},  // an intermediate byte
		{"\x1b[" + strings.Repeat("1", 40) + "mx\r", "x"},
		{"é世😀\r", "é世😀"},
		{"\x80a\xffb\r", "ab"},
		{"ab\x7f\r", "a"},
		{"abc\x01\x04\r", "bc"},
		{"abc\x01\x1b[3~\r", "bc"},
		{"ab cd\x17x\r", "ab x"},
		{"ab\x0b\r", "ab"},
		{"ab\x15c\r", "c"},
		{"ab\x7f\x7f\x7f\r", ""},
		{"a\x00b\r", "ab"},
	} {
		l, err, _ := readOne(t, c.in)
		if err != nil || l != c.want {
			t.Errorf("%q: %q, %v; want %q", c.in, l, err, c.want)
		}
	}
}

func TestReadLineEnds(t *testing.T) {
	l, err, out := readOne(t, "ab\x03")
	if l != "" || !errors.Is(err, ErrInterrupt) || !strings.HasSuffix(out, "\r\n") || !strings.Contains(out, "ab^C") {
		t.Fatalf("^C: %q %v %q", l, err, out)
	}
	if l, err, out = readOne(t, "\x04"); l != "" || err != io.EOF || !strings.HasSuffix(out, "\r\n") {
		t.Fatalf("^D: %q %v %q", l, err, out)
	}
	if l, err, _ = readOne(t, ""); l != "" || err != io.EOF {
		t.Fatalf("empty input: %q %v", l, err)
	}
	r, _ := newReader("ab")
	if l, err = r.ReadLine(context.Background(), "$ "); l != "ab" || err != nil {
		t.Fatalf("EOF after text: %q %v", l, err)
	}
	if h := r.Editor().History(); len(h) != 1 || h[0] != "ab" {
		t.Fatalf("history %q", h)
	}
	boom := errors.New("boom")
	r = &Reader{In: &failing{data: "ab", err: boom}, Out: io.Discard}
	if l, err = r.ReadLine(context.Background(), "$ "); l != "" || err != boom {
		t.Fatalf("read error: %q %v", l, err)
	}
	// Input ending inside a sequence is an error, not a key.
	for _, in := range []string{"\x1b", "\x1b[", "\x1b[1", "\x1bO", "\xc3", "\xf0\x9f"} {
		if l, err, _ = readOne(t, in); l != "" || err != io.EOF {
			t.Errorf("%q: %q %v", in, l, err)
		}
	}
}

func TestReadLineHistoryAndSearch(t *testing.T) {
	r, _ := newReader("one\rtwo\r\x1b[A\x1b[A\x1b[B\r\x12on\r\x10\r")
	ctx := context.Background()
	for _, want := range []string{"one", "two", "two", "one"} {
		if l, err := r.ReadLine(ctx, "$ "); err != nil || l != want {
			t.Fatalf("%q %v, want %q", l, err, want)
		}
	}
	if l, err := r.ReadLine(ctx, "$ "); err != nil || l != "one" {
		t.Fatalf("^P: %q %v", l, err)
	}
	r = &Reader{In: strings.NewReader("a\rb\rc\r"), Out: io.Discard, HistorySize: 2}
	for range 3 {
		r.ReadLine(ctx, "$ ")
	}
	if h := r.Editor().History(); len(h) != 2 || h[0] != "b" {
		t.Fatalf("bounded history %q", h)
	}
}

func TestReadLineOutput(t *testing.T) {
	ctx := context.Background()
	out := &bytes.Buffer{}
	r := &Reader{In: &flaky{r: strings.NewReader("a\x0c\x7f\x7f\r")}, Out: out, Cols: func() int { return 30 }}
	if l, err := r.ReadLine(ctx, "head\n$ "); err != nil || l != "" {
		t.Fatalf("%q %v", l, err)
	}
	s := out.String()
	if !strings.HasPrefix(s, "head\n$ ") || !strings.Contains(s, "\x1b[H\x1b[2J") || !strings.Contains(s, "\a") {
		t.Fatalf("output %q", s)
	}
	out.Reset()
	r = &Reader{In: strings.NewReader("\r"), Out: out, PromptStyle: "\x1b[1m"}
	r.ReadLine(ctx, "$ ")
	if !strings.Contains(out.String(), "\x1b[1m$ \x1b[0m") {
		t.Fatalf("styled prompt: %q", out.String())
	}
	// A line exactly as wide as the terminal ends on a fresh row.
	out.Reset()
	r = &Reader{In: strings.NewReader("abcdefgh\r"), Out: out, Cols: func() int { return 10 }}
	if l, _ := r.ReadLine(ctx, "$ "); l != "abcdefgh" {
		t.Fatal(l)
	}
	// A search result is shown as the line it submits.
	out.Reset()
	r = &Reader{In: strings.NewReader("xyz\r\x12xy\r"), Out: out}
	r.ReadLine(ctx, "$ ")
	out.Reset()
	if l, _ := r.ReadLine(ctx, "$ "); l != "xyz" || !strings.Contains(out.String(), "$ xyz") {
		t.Fatalf("%q %q", l, out.String())
	}
}

func TestReadLineCompletion(t *testing.T) {
	ctx := context.Background()
	var many []Candidate
	for i := 0; i < 150; i++ {
		many = append(many, Candidate{Value: "c" + strconv.Itoa(i)})
	}
	comp := static(map[string][]Candidate{
		"":  {{Value: "wctr"}, {Value: "wc"}},
		"x": many,
	})
	out := &bytes.Buffer{}
	for _, in := range []string{"w\t\t\r", "x \t\ty\r", "x \t\tn\r", "q\t\r"} {
		out.Reset()
		r := &Reader{In: strings.NewReader(in), Out: out, Complete: comp}
		l, err := r.ReadLine(ctx, "$ ")
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		s := out.String()
		switch in[0] {
		case 'w':
			if l != "wc" || !strings.Contains(s, "wctr") {
				t.Errorf("%q: %q %q", in, l, s)
			}
		case 'x':
			if !strings.Contains(s, "Display all 150") || (strings.Contains(in, "y") != strings.Contains(s, "c149")) {
				t.Errorf("%q: %q", in, s)
			}
		case 'q':
			if !strings.Contains(s, "\a") {
				t.Errorf("no candidate rings: %q", s)
			}
		}
	}
	r := &Reader{In: strings.NewReader("\t\r"), Out: out}
	if l, err := r.ReadLine(ctx, "$ "); err != nil || l != "" {
		t.Fatal(l, err)
	}
}

func FuzzReader(f *testing.F) {
	for _, s := range []string{"ls\r", "\x1b[A\x1b[B\x1b[C\x1b[D", "é世\x03", "a\tb\t\t", "\x12a\x12\x12\r", "\x1b[1;5", "\xff\xfe\x1b\x1bO"} {
		f.Add([]byte(s))
	}
	comp := static(map[string][]Candidate{"": {{Value: "a"}, {Value: "ab"}, {Value: "b", Description: "bee"}}})
	f.Fuzz(func(t *testing.T, in []byte) {
		out := &bytes.Buffer{}
		r := &Reader{In: bytes.NewReader(in), Out: out, Complete: comp, HistorySize: 4, Cols: func() int { return 7 }}
		for range len(in) + 2 {
			l, err := r.ReadLine(context.Background(), "$ ")
			if err == io.EOF {
				return
			}
			if err != nil && !errors.Is(err, ErrInterrupt) {
				t.Fatal(err)
			}
			if !utf8.ValidString(l) || utf8.RuneCountInString(l) > MaxLine {
				t.Fatalf("line %q", l)
			}
		}
		t.Fatal("ReadLine does not consume its input")
	})
}

func TestMoreEditor(t *testing.T) {
	ctx := context.Background()
	e := New(Options{})
	for range MaxLine + 5 {
		e.Key(ctx, R('a'))
	}
	if n := len(e.State().Line); n != MaxLine {
		t.Fatalf("line of %d", n)
	}
	e.Reset()
	if ev := e.Key(ctx, R('\a')); !reflect.DeepEqual(ev, Event{}) || e.State().Line != "" {
		t.Fatal("a control rune is not text")
	}
	if ev := typ(e, "\x1b[3~"); !ev[0].Bell {
		t.Fatal("delete at the end rings")
	}
	e.SetPrompt("")
	e.SetPrompt("> ")
	e.SetCols(0)
	e.SetCols(30)
	if e.State().Prompt != "> " || e.opt.Cols != 30 {
		t.Fatal("prompt or width not set")
	}
	// A completion that would overflow the line rings.
	e = New(Options{Complete: func(_ context.Context, _ []string, cur string) ([]Candidate, bool) {
		return []Candidate{{Value: cur + "bbbbbbbbbb"}}, false
	}})
	for range MaxLine - 5 {
		e.Key(ctx, R('a'))
	}
	if ev := typ(e, "\t"); !ev[0].Bell {
		t.Fatal("overflowing completion rings")
	}
	// Ctrl-R finds nothing more to go back to.
	e = New(Options{})
	e.SetHistory([]string{"zz", "a"})
	typ(e, "\x12\x12")
	if ev := typ(e, "a"); !reflect.DeepEqual(ev[0], Event{}) || e.State().Line != "a" {
		t.Fatal("search for a")
	}
	if ev := typ(e, "\x12"); !ev[0].Bell {
		t.Fatal("no older match rings")
	}
}

func TestWidth(t *testing.T) {
	for r, want := range map[rune]int{'a': 1, 'é': 1, 0x301: 0, 0x200d: 0, '世': 2, '😀': 2, 'ア': 2, 0x400: 1, 0x2000: 1} {
		if got := RuneWidth(r); got != want {
			t.Errorf("RuneWidth(%q) = %d, want %d", r, got, want)
		}
	}
	if Width("a世é́") != 4 {
		t.Fatal("Width")
	}
	var rd Renderer
	rd.Cols = 10
	if s := rd.Render(State{Prompt: "$ ", Line: "世世", Cursor: 1}); !strings.HasSuffix(s, "\r\x1b[4C") {
		t.Fatalf("wide cursor: %q", s)
	}
	if s := rd.Render(State{Prompt: "$ ", Line: "ab", Cursor: 99}); !strings.HasSuffix(s, "\r\x1b[4C") {
		t.Fatalf("cursor past the end: %q", s)
	}
	if got := Format([]Candidate{{Value: "世"}, {Value: "a"}}, 5); len(got) == 0 {
		t.Fatal("small width")
	}
	if got := Format([]Candidate{{Value: "世", Description: "d"}, {Value: "a", Description: "e"}}, 40); got[1] != "a   -- e" {
		t.Fatalf("aligned wide: %q", got)
	}
	// A line as wide as the terminal ends on a fresh row.
	var r2 Renderer
	r2.Cols = 10
	s := State{Prompt: "$ ", Line: "abcdefgh", Cursor: 8}
	r2.Render(s)
	if got := r2.Leave(s); got != "\r\x1b[K" {
		t.Fatalf("Leave: %q", got)
	}
}

func TestParseKeysSequences(t *testing.T) {
	for in, want := range map[string][]KeyCode{
		"\x1b[1 q":  {KeyUnknown},
		"\x1b[?25h": {KeyUnknown},
		"\x1b[1;5D": {KeyLeft},
		"\x1bOA":    {KeyUp},
		"\x1b[":     {KeyEscape, KeyRune},
	} {
		var got []KeyCode
		for _, k := range ParseKeys(in) {
			got = append(got, k.Code)
		}
		if len(got) != len(want) {
			t.Errorf("%q: %v, want %v", in, got, want)
			continue
		}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%q: %v, want %v", in, got, want)
			}
		}
	}
}
