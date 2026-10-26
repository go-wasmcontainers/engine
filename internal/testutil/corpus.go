package testutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// WriteCorpus writes seeds as committed fuzz corpus files under
// testdata/fuzz/<fuzzName> of the calling package, in the "go test fuzz v1"
// encoding, when WRITE_FUZZ_CORPUS=1. Each value is a []byte or a string.
// Normal runs skip: the files are then read by `go test` as seeds.
func WriteCorpus(t *testing.T, fuzzName string, seeds ...any) {
	if os.Getenv("WRITE_FUZZ_CORPUS") != "1" {
		t.Skip("set WRITE_FUZZ_CORPUS=1 to regenerate the committed corpus")
	}
	dir := filepath.Join("testdata", "fuzz", fuzzName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i, s := range seeds {
		var line string
		switch v := s.(type) {
		case []byte:
			line = "[]byte(" + strconv.Quote(string(v)) + ")"
		case string:
			line = "string(" + strconv.Quote(v) + ")"
		default:
			t.Fatalf("seed %d: unsupported type %T", i, s)
		}
		name := filepath.Join(dir, fmt.Sprintf("seed-%02d", i))
		if err := os.WriteFile(name, []byte("go test fuzz v1\n"+line+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
