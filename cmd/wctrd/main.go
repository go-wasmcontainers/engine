// Command wctrd is the engine's daemon: it serves an API compatible with
// (a subset of) the Docker Engine API on a unix socket and runs wasip1
// containers with wazero.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"syscall"
	"time"

	units "github.com/docker/go-units"
	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/api/server"
	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/daemon"
	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/netdriver"
	"github.com/go-wasmcontainers/engine/network"
	"github.com/go-wasmcontainers/engine/runtime/wazero"
	"github.com/go-wasmcontainers/engine/state"
)

var (
	osExit  = os.Exit
	version = "dev"
	commit  = ""
)

// init takes the module version `go install …@vX.Y.Z` recorded, when no
// -ldflags set one.
func init() {
	if bi, ok := debug.ReadBuildInfo(); ok && version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
}

func main() {
	cmd := RootCmd()
	if err := cmd.Execute(); err != nil {
		fmt.Fprintln(cmd.ErrOrStderr(), "wctrd:", err)
		osExit(1)
	}
}

func defaultDataRoot() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "wasmcontainers")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "wasmcontainers")
	}
	return filepath.Join(os.TempDir(), "wasmcontainers")
}

// Config is the daemon's command-line configuration.
type Config struct {
	Host           string
	DataRoot       string
	Memory         string
	Timeout        time.Duration
	Insecure       []string
	MaxContainers  int
	ContainerQuota string
	// WASI is the host serving wasi_snapshot_preview1: "engine" (the
	// engine's own, with sockets) or "wazero" (wazero's, no network).
	WASI string
	// Relay joins a class network: the relay's API base URL, a file
	// holding the join code (never on the command line), the network name
	// containers use (--network NAME), and a CA certificate file to trust.
	Relay, RelayCodeFile, RelayNetwork, RelayCA string
}

// RootCmd builds the command; ready, when not nil, receives the socket
// path once it accepts connections (tests use it).
func RootCmd() *cobra.Command { return rootCmd(nil) }

func rootCmd(ready chan<- string) *cobra.Command {
	var cfg Config
	cmd := &cobra.Command{
		Use:           "wctrd",
		Short:         "Serve the go-wasmcontainers engine: WebAssembly (wasip1) containers behind an API compatible with the Docker Engine API",
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return serve(ctx, cfg, cmd.OutOrStdout(), ready)
		},
	}
	f := cmd.Flags()
	f.StringVarP(&cfg.Host, "host", "H", "unix://"+api.DefaultSocket, "unix socket to listen on (unix:///path)")
	f.StringVar(&cfg.DataRoot, "data-root", defaultDataRoot(), "directory holding the image store")
	f.StringVar(&cfg.Memory, "memory", "256m", "default linear-memory limit per container")
	f.DurationVar(&cfg.Timeout, "run-timeout", 10*time.Minute, "wall-clock limit of one container run (0 = none)")
	f.StringSliceVar(&cfg.Insecure, "insecure-registry", nil, "registry host:port reached over plain HTTP (loopback always is)")
	f.IntVar(&cfg.MaxContainers, "max-containers", 256, "maximum number of containers")
	f.StringVar(&cfg.ContainerQuota, "storage-quota", "256m", "bytes one container may write to its filesystem")
	f.StringVar(&cfg.WASI, "wasi", "engine", "WASI host: engine (with sockets: networks) or wazero (no network)")
	f.StringVar(&cfg.Relay, "relay", "", "join a class network through this relay (https://host:port)")
	f.StringVar(&cfg.RelayCodeFile, "relay-join-code-file", "", "file holding the class's join code (read once; the session token is kept in the data root)")
	f.StringVar(&cfg.RelayNetwork, "relay-network", "class", "name of the class network (run --network NAME)")
	f.StringVar(&cfg.RelayCA, "relay-ca", "", "PEM file of the CA that signed the relay's certificate (wctr-relay ca export)")
	return cmd
}

// listenUnix binds the socket inside a fresh 0700 directory next to its
// final path, makes it 0600, and only then renames it into place: nobody
// else can reach the API at any moment, and the parent directory (often a
// shared one such as /var/run) is never created loosely nor re-permissioned.
func listenUnix(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	st, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket; refusing to replace it", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("another daemon is listening on %s", path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	}
	tmp, err := os.MkdirTemp(dir, ".wctrd-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)
	if err := os.Chmod(tmp, 0o700); err != nil {
		return nil, err
	}
	staged := filepath.Join(tmp, "s")
	l, err := net.Listen("unix", staged)
	if err != nil {
		return nil, err
	}
	ul := l.(*net.UnixListener)
	// The listener's path changes with the rename; the caller removes the
	// final path itself.
	ul.SetUnlinkOnClose(false)
	if err := os.Chmod(staged, 0o600); err != nil {
		l.Close()
		return nil, err
	}
	if err := os.Rename(staged, path); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

func serve(ctx context.Context, cfg Config, out io.Writer, ready chan<- string) error {
	u, err := url.Parse(cfg.Host)
	if err != nil || u.Scheme != "unix" || u.Path == "" {
		return fmt.Errorf("unsupported --host %q: only unix:///path is supported (the API has no authentication, so it is never served over TCP)", cfg.Host)
	}
	mem, err := units.RAMInBytes(cfg.Memory)
	if err != nil {
		return fmt.Errorf("--memory: %w", err)
	}
	quota, err := units.RAMInBytes(cfg.ContainerQuota)
	if err != nil {
		return fmt.Errorf("--storage-quota: %w", err)
	}
	if cfg.WASI != "engine" && cfg.WASI != "wazero" {
		return fmt.Errorf("--wasi %q: engine or wazero", cfg.WASI)
	}
	exec := wazero.New(wazero.Options{DefaultMemoryLimitBytes: mem, EngineWASI: cfg.WASI == "engine"})
	defer exec.Close(context.Background())
	var nopt network.Options
	if cfg.Relay != "" {
		if cfg.WASI != "engine" {
			return errors.New("--relay needs --wasi engine (sockets)")
		}
		j, err := joinRelay(ctx, cfg, out)
		if err != nil {
			return err
		}
		defer j.Close()
		nopt.Drivers = map[string]netdriver.NetworkDriver{cfg.RelayNetwork: j}
		nopt.DriverName = "relay"
	}
	copt := container.Options{Executor: exec, MemoryLimitBytes: mem, Timeout: cfg.Timeout, MaxContainers: cfg.MaxContainers}
	copt.Quota.MaxBytes = quota
	// Containers, their logs and writable layers survive a restart.
	stateDir := filepath.Join(cfg.DataRoot, "containers")
	if copt.Store, err = state.OpenDir(stateDir); err != nil {
		return err
	}
	d, err := daemon.New(daemon.Options{
		Root:       cfg.DataRoot,
		Registry:   image.RegistryOptions{Insecure: cfg.Insecure, UserAgent: "wctrd/" + version},
		Containers: copt,
		Networks:   nopt,
	})
	if err != nil {
		return err
	}
	if r := d.Restored(); r.Restored > 0 || len(r.Repaired) > 0 {
		fmt.Fprintf(out, "wctrd: restored %d containers (%d were running and are now exited), removed %d --rm containers\n", r.Restored, r.Stopped, len(r.Removed))
		for _, m := range r.Repaired {
			fmt.Fprintln(out, "wctrd: repaired:", m)
		}
	}
	l, err := listenUnix(u.Path)
	if err != nil {
		return err
	}
	defer os.Remove(u.Path)
	srv := &http.Server{
		Handler:           server.New(d, server.Info{Version: version, GitCommit: commit}),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	fmt.Fprintf(out, "wctrd %s listening on unix://%s (data root %s)\n", version, u.Path, cfg.DataRoot)
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	if ready != nil {
		ready <- u.Path
	}
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	fmt.Fprintln(out, "wctrd: shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d.Close(sctx)
	srv.Shutdown(sctx)
	return nil
}
