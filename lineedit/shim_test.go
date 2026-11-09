package lineedit

import (
	"errors"
	"reflect"
	"testing"

	gw "github.com/go-widgets/lineedit"
)

// The shim is the same implementation under the old path: a value of one is
// a value of the other, the errors are the same, and the functions behave
// as go-widgets/lineedit's do.
func TestTheShimIsGoWidgetsLineedit(t *testing.T) {
	var e *gw.Editor = New(Options{Prompt: "$ "}) // same type
	if e == nil {
		t.Fatal("New returned nil")
	}
	var k gw.Key = R('x')
	if k.Code != KeyRune || KeyUnknown != gw.KeyUnknown || MaxLine != gw.MaxLine {
		t.Fatalf("key %+v, constants differ", k)
	}
	if !errors.Is(ErrInterrupt, gw.ErrInterrupt) || !errors.Is(ErrUnterminated, gw.ErrUnterminated) {
		t.Fatal("errors are not the same values")
	}
	words, err := Split(`a "b c"`)
	if err != nil || len(words) != 2 || words[1] != "b c" {
		t.Fatalf("Split = %q, %v", words, err)
	}
	if _, err := Split(`"open`); !errors.Is(err, ErrUnterminated) {
		t.Fatalf("Split of an open quote: %v", err)
	}
	if len(ParseKeys("a\r")) != 2 || Width("é") != 1 || RuneWidth('中') != 2 {
		t.Fatal("ParseKeys/Width/RuneWidth differ")
	}
	if Quote("a b", 0, false) != gw.Quote("a b", 0, false) ||
		!reflect.DeepEqual(SplitPartial("x 'y"), gw.SplitPartial("x 'y")) {
		t.Fatal("Quote/SplitPartial differ")
	}
	if got := Format([]Candidate{{Value: "one"}, {Value: "two"}}, 80); len(got) == 0 {
		t.Fatal("Format listed nothing")
	}
}
