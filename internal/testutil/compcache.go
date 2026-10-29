package testutil

import (
	"os"
	"sync"

	"github.com/tetratelabs/wazero"
)

// CompilationCacheEnv names a directory where the tests keep wazero's
// machine code, shared by the test processes of every package (they run
// in parallel; wazero writes each entry to a temporary file and renames
// it) and, in CI, across runs.
const CompilationCacheEnv = "WCTR_TEST_COMPILATION_CACHE"

var (
	compOnce  sync.Once
	compCache wazero.CompilationCache
)

// CompilationCache is the process-wide compilation cache tests hand to
// their executors (wazero.Options.CompilationCache): a guest is compiled
// once per process, or once per machine when CompilationCacheEnv is set,
// instead of once per executor. Compiling Go guests is most of what the
// engine's tests spent under -race.
func CompilationCache() wazero.CompilationCache {
	compOnce.Do(func() {
		if dir := os.Getenv(CompilationCacheEnv); dir != "" {
			if c, err := wazero.NewCompilationCacheWithDir(dir); err == nil {
				compCache = c
				return
			}
		}
		compCache = wazero.NewCompilationCache()
	})
	return compCache
}
