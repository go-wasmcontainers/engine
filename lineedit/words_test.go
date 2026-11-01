package lineedit

import (
	"reflect"
	"strings"
	"testing"
)

func TestSplit(t *testing.T) {
	for in, want := range map[string][]string{
		"":                               nil,
		"   ":                            nil,
		"wctr ps -a":                     {"wctr", "ps", "-a"},
		"  wctr\trun  hello ":            {"wctr", "run", "hello"},
		`wctr run lab echo 'a  b' "c d"`: {"wctr", "run", "lab", "echo", "a  b", "c d"},
		`x "a\"b" 'it'\''s' \ y`:         {"x", `a"b`, "it's", " y"},
		`""`:                             {""},
		`a"b"c`:                          {"abc"},
		`"\n"`:                           {`\n`},
	} {
		got, err := Split(in)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q %v, want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`'open`, `"open`, `trailing\`, strings.Repeat("a", MaxLine+1)} {
		if _, err := Split(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func FuzzSplit(f *testing.F) {
	f.Add(`wctr run lab echo 'a b' "c \" d" e\ f`)
	f.Fuzz(func(t *testing.T, s string) {
		words, err := Split(s)
		if err != nil {
			return
		}
		total := 0
		for _, w := range words {
			total += len(w)
		}
		if total > len(s) {
			t.Fatal("words longer than the line")
		}
	})
}
