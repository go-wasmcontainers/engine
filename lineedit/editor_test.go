package lineedit

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// static completes from a fixed table keyed by the joined words.
func static(table map[string][]Candidate) Completer {
	return func(_ context.Context, words []string, current string) ([]Candidate, bool) {
		var out []Candidate
		for _, c := range table[strings.Join(words, " ")] {
			if strings.HasPrefix(c.Value, current) {
				out = append(out, c)
			}
		}
		return out, false
	}
}

func typ(e *Editor, s string) []Event {
	var evs []Event
	for _, k := range ParseKeys(s) {
		evs = append(evs, e.Key(context.Background(), k))
	}
	return evs
}

func check(t *testing.T, e *Editor, line string, cursor int) {
	t.Helper()
	s := e.State()
	if s.Line != line || s.Cursor != cursor {
		t.Fatalf("line %q cursor %d, want %q %d", s.Line, s.Cursor, line, cursor)
	}
}

func TestEditing(t *testing.T) {
	e := New(Options{})
	typ(e, "wctr ps")
	check(t, e, "wctr ps", 7)
	typ(e, "\x1b[D\x1b[D\x1b[D") // Left ×3
	check(t, e, "wctr ps", 4)
	typ(e, "X")
	check(t, e, "wctrX ps", 5)
	typ(e, "\x7f") // Backspace
	check(t, e, "wctr ps", 4)
	typ(e, "\x1b[3~") // Delete
	check(t, e, "wctrps", 4)
	typ(e, " ")
	typ(e, "\x01") // Ctrl-A
	check(t, e, "wctr ps", 0)
	if ev := typ(e, "\x7f"); !ev[0].Bell {
		t.Fatal("backspace at 0 rings")
	}
	typ(e, "\x05") // Ctrl-E
	check(t, e, "wctr ps", 7)
	typ(e, " -a")
	typ(e, "\x17") // Ctrl-W
	check(t, e, "wctr ps ", 8)
	typ(e, "\x17\x17")
	check(t, e, "", 0)
	typ(e, "abc def\x1b[D\x1b[D\x1b[D")
	typ(e, "\x0b") // Ctrl-K
	check(t, e, "abc ", 4)
	typ(e, "\x15") // Ctrl-U
	check(t, e, "", 0)
	typ(e, "x\x1b[H") // Home
	check(t, e, "x", 0)
	typ(e, "\x1b[F\x02\x06") // End, Ctrl-B, Ctrl-F
	check(t, e, "x", 1)
	typ(e, "\x1b[1;5D") // Ctrl-Left folds to Left
	check(t, e, "x", 0)
	if ev := typ(e, "\x0c"); !ev[0].Clear {
		t.Fatal("ctrl-l")
	}
	if ev := typ(e, "\x03"); !ev[0].Interrupt {
		t.Fatal("ctrl-c")
	}
	check(t, e, "", 0)
	if ev := typ(e, "\x04"); !ev[0].EOF {
		t.Fatal("ctrl-d on an empty line")
	}
	typ(e, "ab\x01\x04")
	check(t, e, "b", 0)
	ev := typ(e, "\r")
	if !ev[0].Submit || ev[0].Line != "b" {
		t.Fatalf("%+v", ev[0])
	}
	check(t, e, "", 0)
	// Unknown keys and controls never become text.
	typ(e, "\x1b[99~\x1bq\x00\u009b")
	check(t, e, "q", 1)
}

func TestHistory(t *testing.T) {
	e := New(Options{HistorySize: 3})
	for _, l := range []string{"one", "two", "two", "three", "four", "   "} {
		typ(e, l+"\r")
	}
	if h := strings.Join(e.History(), ","); h != "two,three,four" {
		t.Fatal(h)
	}
	typ(e, "draft")
	typ(e, "\x1b[A")
	check(t, e, "four", 4)
	typ(e, "\x1b[A\x1b[A")
	check(t, e, "two", 3)
	if ev := typ(e, "\x1b[A"); !ev[0].Bell {
		t.Fatal("past the oldest")
	}
	typ(e, "\x0e\x0e\x0e") // Ctrl-N ×3
	check(t, e, "draft", 5)
	if ev := typ(e, "\x1b[B"); !ev[0].Bell {
		t.Fatal("past the newest")
	}
	e.SetHistory([]string{"a", "b", "c", "d", ""})
	if h := strings.Join(e.History(), ","); h != "b,c,d" {
		t.Fatal(h)
	}
	// A restored history is untrusted: what nobody could type is dropped
	// before the renderer can write it.
	e.SetHistory([]string{"ok", "\x1b]52;c;ZXZpbA==\a", "bad\xff", "tab\there", "café ü"})
	if h := strings.Join(e.History(), ","); h != "ok,café ü" {
		t.Fatalf("%q", h)
	}
}

func TestReverseSearch(t *testing.T) {
	e := New(Options{})
	e.SetHistory([]string{"wctr run --rm hello", "wctr ps -a", "wctr run lab echo hi"})
	typ(e, "\x12run")
	s := e.State()
	if !strings.HasPrefix(s.Prompt, "(reverse-i-search)`run'") || s.Line != "wctr run lab echo hi" {
		t.Fatalf("%+v", s)
	}
	typ(e, "\x12")
	if e.State().Line != "wctr run --rm hello" {
		t.Fatal(e.State())
	}
	if ev := typ(e, "\x12"); !ev[0].Bell {
		t.Fatal("no older match rings")
	}
	typ(e, "\x7fx")  // backspace then a query with no match
	typ(e, "\x1b[C") // Right accepts (the last match)
	if e.State().Prompt != "$ " {
		t.Fatal("still searching")
	}
	typ(e, "\x15\x12ps")
	ev := typ(e, "\r")
	if !ev[0].Submit || ev[0].Line != "wctr ps -a" {
		t.Fatalf("%+v", ev[0])
	}
	typ(e, "keep\x12zz\x07") // Ctrl-G cancels
	check(t, e, "keep", 4)
}

func TestTabCompletion(t *testing.T) {
	cands := map[string][]Candidate{
		"":               {{Value: "wctr"}, {Value: "help"}},
		"wctr":           {{Value: "run", Description: "Create and run"}, {Value: "rm", Description: "Remove"}, {Value: "rmi", Description: "Remove images"}, {Value: "ps"}},
		"wctr run":       {{Value: "hello"}, {Value: "hello:latest"}, {Value: "two words"}},
		"wctr run hello": {},
	}
	e := New(Options{Complete: static(cands), Cols: 60})
	typ(e, "wc\t")
	check(t, e, "wctr ", 5)
	typ(e, "r\t")
	check(t, e, "wctr r", 6) // run, rm, rmi: nothing common to add
	ev := typ(e, "\t")
	if len(ev[0].Print) != 3 || !strings.Contains(ev[0].Print[0], "run  -- Create and run") {
		t.Fatalf("%q", ev[0].Print)
	}
	typ(e, "u\t")
	check(t, e, "wctr run ", 9)
	typ(e, "he\t")
	check(t, e, "wctr run hello", 14) // common prefix of hello, hello:latest
	if ev := typ(e, "\t"); len(ev[0].Print) != 1 || strings.Join(strings.Fields(ev[0].Print[0]), " ") != "hello hello:latest" {
		t.Fatalf("%q", ev[0].Print)
	}
	// Quoting: a value with a blank is escaped; inside an open quote it is
	// closed.
	e.Reset()
	typ(e, "wctr run t\t")
	check(t, e, `wctr run two\ words `, 20)
	e.Reset()
	typ(e, "wctr run 'tw\t")
	check(t, e, `wctr run 'two words' `, 21)
	// Mid-line: completes the word before the cursor, keeps the rest.
	e.Reset()
	typ(e, "wctr ru --rm\x1b[D\x1b[D\x1b[D\x1b[D\x1b[D\t")
	check(t, e, "wctr run  --rm", 9)
	// Nothing to complete rings.
	e.Reset()
	if ev := typ(e, "zzz\t"); !ev[3].Bell {
		t.Fatal("bell")
	}
	if ev := New(Options{}).Key(context.Background(), Key{Code: KeyTab}); !ev.Bell {
		t.Fatal("no completer rings")
	}
}

func TestManyCandidates(t *testing.T) {
	var many []Candidate
	for i := 0; i < 150; i++ {
		many = append(many, Candidate{Value: "c" + strconv.Itoa(i)})
	}
	e := New(Options{Complete: static(map[string][]Candidate{"x": many})})
	typ(e, "x c\t")
	ev := typ(e, "\t")
	if len(ev[0].Print) != 1 || ev[0].Print[0] != "Display all 150 possibilities? (y or n)" {
		t.Fatalf("%q", ev[0].Print)
	}
	ev = typ(e, "y")
	if len(ev[0].Print) < 10 {
		t.Fatalf("%d lines", len(ev[0].Print))
	}
	typ(e, "\t\t")
	ev = typ(e, "n")
	if len(ev[0].Print) != 0 {
		t.Fatal("n lists")
	}
	check(t, e, "x c", 3)
}

func TestSanitizeAndFormat(t *testing.T) {
	cands := []Candidate{{Value: "ok", Description: "a\x1b]52;c;x\x07b\tc"}, {Value: "bad\x1bx"}, {Value: "\xff"}, {Value: ""}}
	var kept []Candidate
	for _, c := range cands {
		if cc, ok := sanitize(c); ok {
			kept = append(kept, cc)
		}
	}
	if len(kept) != 1 || kept[0].Description != "a]52;c;xb c" {
		t.Fatalf("%q", kept)
	}
	if got := Format([]Candidate{{Value: "a"}, {Value: "bb"}, {Value: "ccc"}, {Value: "d"}}, 20); strings.Join(got, "|") != "a    ccc|bb   d" {
		t.Fatalf("%q", got)
	}
	if got := Format([]Candidate{{Value: "x", Description: strings.Repeat("y", 100)}}, 30); len([]rune(got[0])) != 29 {
		t.Fatalf("%q", got)
	}
	if commonPrefix([]Candidate{{Value: "héllo"}, {Value: "hé"}}) != "hé" || commonPrefix(nil) != "" {
		t.Fatal("lcp")
	}
	long := strings.Repeat("d", 300)
	if c, _ := sanitize(Candidate{Value: "v", Description: long}); len(c.Description) != 200 {
		t.Fatal("bound")
	}
}

func FuzzEditor(f *testing.F) {
	f.Add("wctr r\t\t\x1b[D\x7f\x12ru\r")
	f.Fuzz(func(t *testing.T, input string) {
		e := New(Options{Complete: static(map[string][]Candidate{"": {{Value: "wctr"}}, "wctr": {{Value: "run"}, {Value: "rm"}}})})
		r := &Renderer{Cols: 20}
		for _, k := range ParseKeys(input) {
			ev := e.Key(context.Background(), k)
			s := e.State()
			if s.Cursor < 0 || s.Cursor > len([]rune(s.Line)) {
				t.Fatalf("cursor %d outside %q", s.Cursor, s.Line)
			}
			if ev.Print != nil {
				r.Print(ev.Print, s)
			} else {
				r.Render(s)
			}
		}
	})
}

var _ = fmt.Sprint
