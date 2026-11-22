package browser

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNoSourceEvaluatesAString: the page's Content-Security-Policy is
// script-src 'self' 'wasm-unsafe-eval', so Function(...) or eval(...) panics
// the engine worker at start (playground#63, engine v0.42.0: every browser
// lane died on "Evaluating a string as JavaScript violates ..."). Scripts
// reach the worker as served files (importScripts), never as strings.
func TestNoSourceEvaluatesAString(t *testing.T) {
	files, err := filepath.Glob("*")
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || (!strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, ".js")) {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		seen++
		for n, line := range strings.Split(string(b), "\n") {
			if c := strings.TrimSpace(line); strings.HasPrefix(c, "//") {
				continue
			}
			for _, bad := range []string{`Get("Function")`, `Get("eval")`, "new Function(", "eval(", "Function("} {
				if strings.Contains(line, bad) {
					t.Errorf("%s:%d evaluates a string (%s): %s", f, n+1, bad, strings.TrimSpace(line))
				}
			}
		}
	}
	if seen < 5 {
		t.Fatalf("scanned %d sources: the glob read nothing", seen)
	}
}
