// Command dockerd is the engine's daemon: it serves the engine's subset of
// the Docker Engine API on a unix socket and runs wasip1 containers with
// wazero.
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
	"github.com/go-wasmcontainers/engine/runtime/wazero"
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
		fmt.Fprintln(cmd.ErrOrStderr(), "dockerd:", err)
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
}

// RootCmd builds the command; ready, when not nil, receives the socket
// path once it accepts connections (tests use it).
func RootCmd() *cobra.Command { return rootCmd(nil) }

func rootCmd(ready chan<- string) *cobra.Command {
	var cfg Config
	cmd := &cobra.Command{
		Use:           "dockerd",
		Short:         "Serve the go-wasmcontainers engine: WebAssembly (wasip1) containers behind a Docker API",
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
	f.StringVarP(&cfg.Host, "host", "H", "unix://"+api.DefaultSocket(), "unix socket to listen on (unix:///path)")
	f.StringVar(&cfg.DataRoot, "data-root", defaultDataRoot(), "directory holding the image store")
	f.StringVar(&cfg.Memory, "memory", "256m", "default linear-memory limit per container")
	f.DurationVar(&cfg.Timeout, "run-timeout", 10*time.Minute, "wall-clock limit of one container run (0 = none)")
	f.StringSliceVar(&cfg.Insecure, "insecure-registry", nil, "registry host:port reached over plain HTTP (loopback always is)")
	f.IntVar(&cfg.MaxContainers, "max-containers", 256, "maximum number of containers")
	f.StringVar(&cfg.ContainerQuota, "storage-quota", "256m", "bytes one container may write to its filesystem")
	return cmd
}

// listenUnix creates the socket inside a directory only this user can
// enter, then makes the socket itself 0600: nobody else can reach the API,
// and there is no window during which the socket is open to others.
func listenUnix(path string) (net.Listener, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
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
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
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
	exec := wazero.New(wazero.Options{DefaultMemoryLimitBytes: mem})
	defer exec.Close(context.Background())
	copt := container.Options{Executor: exec, MemoryLimitBytes: mem, Timeout: cfg.Timeout, MaxContainers: cfg.MaxContainers}
	copt.Quota.MaxBytes = quota
	d, err := daemon.New(daemon.Options{
		Root:       cfg.DataRoot,
		Registry:   image.RegistryOptions{Insecure: cfg.Insecure, UserAgent: "go-wasmcontainers-dockerd/" + version},
		Containers: copt,
	})
	if err != nil {
		return err
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
	fmt.Fprintf(out, "dockerd %s listening on unix://%s (data root %s)\n", version, u.Path, cfg.DataRoot)
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
	fmt.Fprintln(out, "dockerd: shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d.Close(sctx)
	srv.Shutdown(sctx)
	return nil
}
