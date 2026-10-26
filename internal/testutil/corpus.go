package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// Tuple is one corpus entry of a fuzz target taking several arguments.
type Tuple []any

func encodeCorpusValue(v any) (string, error) {
	switch x := v.(type) {
	case []byte:
		return "[]byte(" + strconv.Quote(string(x)) + ")", nil
	case string:
		return "string(" + strconv.Quote(x) + ")", nil
	case uint8:
		return fmt.Sprintf("byte(%d)", x), nil
	case uint32:
		return fmt.Sprintf("uint32(%d)", x), nil
	case uint64:
		return fmt.Sprintf("uint64(%d)", x), nil
	case int:
		return fmt.Sprintf("int(%d)", x), nil
	}
	return "", fmt.Errorf("unsupported corpus type %T", v)
}

// WriteCorpus writes seeds as committed fuzz corpus files under
// testdata/fuzz/<fuzzName> of the calling package, in the "go test fuzz v1"
// encoding, when WRITE_FUZZ_CORPUS=1. Each seed is one value ([]byte,
// string, uint8, uint32, uint64, int) or a Tuple of them. Normal runs skip:
// the files are then read by `go test` as seeds.
func WriteCorpus(t *testing.T, fuzzName string, seeds ...any) {
	if os.Getenv("WRITE_FUZZ_CORPUS") != "1" {
		t.Skip("set WRITE_FUZZ_CORPUS=1 to regenerate the committed corpus")
	}
	dir := filepath.Join("testdata", "fuzz", fuzzName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, s := range seeds {
		vals, ok := s.(Tuple)
		if !ok {
			vals = Tuple{s}
		}
		lines := []string{"go test fuzz v1"}
		for _, v := range vals {
			l, err := encodeCorpusValue(v)
			if err != nil {
				t.Fatalf("seed %d: %v", i, err)
			}
			lines = append(lines, l)
		}
		name := filepath.Join(dir, fmt.Sprintf("seed-%02d", i))
		if err := os.WriteFile(name, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
