package lineedit

import (
	"reflect"
	"testing"
)

func TestSplitPartial(t *testing.T) {
	for in, want := range map[string]Partial{
		"":               {Start: 0},
		"wctr":           {Current: "wctr", Start: 0},
		"wctr ":          {Words: []string{"wctr"}, Start: 5},
		"wctr run he":    {Words: []string{"wctr", "run"}, Current: "he", Start: 9},
		`wctr run 'he`:   {Words: []string{"wctr", "run"}, Current: "he", Start: 9, Quote: '\''},
		`wctr run "a\"b`: {Words: []string{"wctr", "run"}, Current: `a"b`, Start: 9, Quote: '"'},
		`wctr run a\ b`:  {Words: []string{"wctr", "run"}, Current: "a b", Start: 9},
		`x 'a b' c`:      {Words: []string{"x", "a b"}, Current: "c", Start: 8},
		`x trailing\`:    {Words: []string{"x"}, Current: "trailing", Start: 2},
		"  wctr  ps  -":  {Words: []string{"wctr", "ps"}, Current: "-", Start: 12},
	} {
		got := SplitPartial(in)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %+v, want %+v", in, got, want)
		}
	}
}

func TestQuoteRoundTrip(t *testing.T) {
	for _, w := range []string{"plain", "two words", `it's`, `a"b`, `$HOME\x`, "", "ünï ✓"} {
		for _, q := range []byte{0, '\'', '"'} {
			if w == "" && q == 0 {
				continue // an empty word needs quotes; completion never inserts one
			}
			s := Quote(w, q, true)
			got, err := Split(s)
			if err != nil || len(got) != 1 || got[0] != w {
				t.Errorf("%q with %q: %q -> %q %v", w, q, s, got, err)
			}
		}
	}
	if Quote("ab", '\'', false) != "'ab" {
		t.Fatal("open quote")
	}
}

func FuzzSplitPartial(f *testing.F) {
	f.Add(`wctr run 'a b' "c\"d" e\ f`)
	f.Fuzz(func(t *testing.T, s string) {
		p := SplitPartial(s)
		if p.Start > len(s) || p.Start < 0 {
			t.Fatal("start out of range")
		}
		// A completed word, quoted, reads back as itself.
		w := p.Current
		q := Quote(w, p.Quote, true)
		if len(q) > MaxLine {
			return
		}
		if got, err := Split(q); err != nil || (w != "" && (len(got) != 1 || got[0] != w)) {
			t.Fatalf("%q: %q %v", w, got, err)
		}
	})
}
