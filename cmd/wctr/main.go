// Command wctr is the engine's client (package cli) over a unix socket,
// with `wctr build` (github.com/go-wasmcontainers/build) and `wctr compose`
// (github.com/go-wasmcontainers/compose) wired in through
// cli.Options.Extra.
//
// It is a module of its own (github.com/go-wasmcontainers/engine/cmd/wctr,
// tags cmd/wctr/vX.Y.Z), because build and compose depend on the engine:
// importing them from the engine's module would make a module cycle. It
// requires released versions of all three, so `go install …@latest` works.
//
// It imports no runtime and nothing native, so it also builds for
// GOOS=wasip1.
package main

import (
	"os"
	"runtime/debug"

	"github.com/spf13/cobra"

	buildcli "github.com/go-wasmcontainers/build/cli"
	composecli "github.com/go-wasmcontainers/compose/cli"
	"github.com/go-wasmcontainers/engine/cli"
)

// osExit is replaced by tests.
var osExit = os.Exit

// version is set at build time with -ldflags "-X main.version=…"; when it
// is not, the version of the engine module this binary was built with
// (what `wctr version` reports beside the daemon's).
var version = "dev"

func init() {
	if version != "dev" {
		return
	}
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, d := range bi.Deps {
			if d.Path == "github.com/go-wasmcontainers/engine" && d.Version != "" && d.Version != "(devel)" {
				version = d.Version
				if d.Replace != nil && d.Replace.Version != "" {
					version = d.Replace.Version
				}
			}
		}
	}
}

// rootCmd is wctr with build and compose. TraverseChildren: the root's
// flags (-H) are read before the subcommand, so each subcommand keeps its
// own short flags (compose's -f is --file, logs' -f is --follow).
func rootCmd() *cobra.Command {
	cmd := cli.RootCmd(cli.Options{Version: version, Extra: []func(cli.Engine) *cobra.Command{
		func(e cli.Engine) *cobra.Command { return buildcli.ForEngine(e) },
		func(e cli.Engine) *cobra.Command { return composecli.Command(composecli.Options{Client: e.Client}) },
	}})
	cmd.TraverseChildren = true
	return cmd
}

func main() {
	cmd := rootCmd()
	if err := cmd.Execute(); err != nil {
		osExit(cli.Report(cmd.ErrOrStderr(), err))
	}
}
