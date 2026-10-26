// Command wctr is the engine's client (package cli) over a unix socket.
//
// It imports no runtime and nothing native, so it also builds for
// GOOS=wasip1.
package main

import (
	"os"
	"runtime/debug"

	"github.com/go-wasmcontainers/engine/cli"
)

// osExit is replaced by tests.
var osExit = os.Exit

// version is set at build time with -ldflags "-X main.version=…"; when it
// is not, the module version `go install …@vX.Y.Z` recorded is used.
var version = "dev"

func init() {
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
}

func main() {
	cmd := cli.RootCmd(cli.Options{Version: version})
	if err := cmd.Execute(); err != nil {
		osExit(cli.Report(cmd.ErrOrStderr(), err))
	}
}
