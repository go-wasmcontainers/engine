// Package cli is the wctr command: subcommands in the familiar container
// CLI grammar (pull, run, ps, logs, rm, rmi, inspect, images) speaking the
// engine's API, which is compatible with the Docker Engine API.
//
// It is a package so that the command runs anywhere a client can reach a
// daemon: cmd/wctr over a unix socket, the browser playground in-process
// through an in-memory transport. It imports no runtime and nothing
// native, so it also builds for GOOS=wasip1 and GOOS=js.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	nethttp "net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	units "github.com/docker/go-units"
	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/image"
)

// Options configure a command tree.
type Options struct {
	// Version is what --version and `wctr version` print (default "dev").
	Version string
	// DefaultHost is the -H default (default api.DefaultHost()).
	DefaultHost string
	// Client connects to a host; nil means api.NewClient.
	Client func(host string) (*api.Client, error)
	// LookupEnv resolves `-e KEY` (default os.LookupEnv).
	LookupEnv func(string) (string, bool)
	// PullCandidates, when set, adds pullable images to the completion of
	// pull and run (the playground's mirror catalogue).
	PullCandidates func(context.Context) []cobra.Completion
	// RunArgs, when set, completes the arguments after the image of run
	// (the playground knows its lab image's subcommands).
	RunArgs func(image string, args []string) []cobra.Completion
	// NoCompletionCommand removes cobra's `completion` command (scripts
	// for a shell the host does not have, as in the browser).
	NoCompletionCommand bool
	// Extra adds commands from other modules (`wctr build`, `wctr
	// compose`) to the tree. Each is made once per RootCmd and receives a
	// way to reach the engine selected by -H: Engine returns its HTTP
	// client and base URL (or an *api.Client through Client).
	Extra []func(Engine) *cobra.Command
}

// Engine is how an extra command reaches the engine -H names.
type Engine struct {
	// Client connects through the CLI's own -H handling.
	Client func() (*api.Client, error)
}

// HTTP returns the engine's HTTP client and base URL (requests go to
// base + "/v1.47/…" or unversioned paths).
func (e Engine) HTTP() (*nethttp.Client, string, error) {
	c, err := e.Client()
	if err != nil {
		return nil, "", err
	}
	hc, base := c.HTTP()
	return hc, base, nil
}

// exitError carries a process exit status through cobra.
type exitError struct {
	code int
	err  error
}

func (e *exitError) Error() string {
	if e.err == nil {
		return fmt.Sprintf("exit status %d", e.code)
	}
	return e.err.Error()
}

func (e *exitError) Unwrap() error { return e.err }

// Exit is a status RunE wants the process to end with (`wctr run`
// returns the container's).
func Exit(code int) error { return &exitError{code: code} }

// Report prints err the way the command does and returns the exit status.
func Report(w io.Writer, err error) int {
	var ee *exitError
	if errors.As(err, &ee) {
		if ee.err != nil {
			fmt.Fprintln(w, "wctr:", ee.err)
		}
		return ee.code
	}
	fmt.Fprintln(w, err)
	return 1
}

// RootCmd builds the command tree.
func RootCmd(o Options) *cobra.Command {
	if o.Version == "" {
		o.Version = "dev"
	}
	if o.DefaultHost == "" {
		o.DefaultHost = api.DefaultHost()
	}
	if o.Client == nil {
		o.Client = api.NewClient
	}
	if o.LookupEnv == nil {
		o.LookupEnv = os.LookupEnv
	}
	var host string
	root := &cobra.Command{
		Use:           "wctr",
		Short:         "Run WebAssembly (wasip1) containers with the go-wasmcontainers engine",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       o.Version,
	}
	root.PersistentFlags().StringVarP(&host, "host", "H", o.DefaultHost, "daemon socket to connect to (unix:///path; default $"+api.HostEnv+", else unix://"+api.DefaultSocket+")")
	client := func() (*api.Client, error) { return o.Client(host) }

	root.AddCommand(
		pullCmd(client), imagesCmd(client), runCmd(client, o.LookupEnv), psCmd(client), portCmd(client), logsCmd(client),
		rmCmd(client), rmiCmd(client), inspectCmd(client), killCmd(client), versionCmd(client, o.Version),
		systemCmd(client), startCmd(client), diffCmd(client), stopCmd(client, false), stopCmd(client, true),
		eventsCmd(client), statsCmd(client), volumeCmd(client), networkCmd(client),
		commitCmd(client), tagCmd(client), execCmd(client, o.LookupEnv),
	)
	for _, mk := range o.Extra {
		if c := mk(Engine{Client: client}); c != nil {
			root.AddCommand(c)
		}
	}
	root.CompletionOptions.DisableDefaultCmd = o.NoCompletionCommand
	(&completer{client: client, opt: o}).install(root)
	return root
}

type clientFunc func() (*api.Client, error)

func pullCmd(client clientFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "pull IMAGE",
		Short: "Pull an image from a registry or an OCI layout (oci:<dir>[:<ref.name>])",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return pull(cmd.Context(), c, cmd.OutOrStdout(), args[0])
		},
	}
}

// absLayout makes an "oci:<dir>…" reference absolute: the daemon resolves
// paths against its own working directory, not the client's.
func absLayout(ref string) string {
	if !image.IsLayoutRef(ref) {
		return ref
	}
	lr, err := image.ParseLayoutRef(ref)
	if err != nil {
		return ref
	}
	dir, err := filepath.Abs(lr.Dir)
	if err != nil {
		return ref
	}
	out := image.LayoutPrefix + dir
	if lr.RefName != "" {
		out += ":" + lr.RefName
	}
	return out
}

func pull(ctx context.Context, c *api.Client, out io.Writer, ref string) error {
	return c.Pull(ctx, absLayout(ref), func(m api.JSONMessage) {
		if m.ID != "" {
			fmt.Fprintf(out, "%s: %s\n", m.ID, m.Status)
		} else {
			fmt.Fprintln(out, m.Status)
		}
	})
}

func splitRepoTag(rt string) (string, string) {
	if i := strings.LastIndexByte(rt, ':'); i > strings.LastIndexByte(rt, '/') {
		return rt[:i], rt[i+1:]
	}
	return rt, "<none>"
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func ago(t time.Time) string { return units.HumanDuration(time.Since(t)) + " ago" }

func imagesCmd(client clientFunc) *cobra.Command {
	var quiet bool
	cmd := &cobra.Command{
		Use:     "images",
		Aliases: []string{"image ls"},
		Short:   "List images",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			imgs, err := c.Images(cmd.Context())
			if err != nil {
				return err
			}
			if quiet {
				for _, i := range imgs {
					fmt.Fprintln(cmd.OutOrStdout(), shortID(i.ID))
				}
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(tw, "REPOSITORY\tTAG\tIMAGE ID\tCREATED\tSIZE")
			for _, i := range imgs {
				tags := i.RepoTags
				if len(tags) == 0 {
					tags = []string{"<none>:<none>"}
				}
				for _, rt := range tags {
					repo, tag := splitRepoTag(rt)
					fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", repo, tag, shortID(i.ID), ago(time.Unix(i.Created, 0)),
						units.HumanSizeWithPrecision(float64(i.Size), 3))
				}
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "only show image IDs")
	return cmd
}

// envFlags resolves -e entries: "KEY=value" as is, a bare "KEY" from this
// process's environment (dropped when unset).
func envFlags(in []string, lookup func(string) (string, bool)) []string {
	var out []string
	for _, e := range in {
		if strings.Contains(e, "=") {
			out = append(out, e)
		} else if v, ok := lookup(e); ok {
			out = append(out, e+"="+v)
		}
	}
	return out
}

func runCmd(client clientFunc, lookup func(string) (string, bool)) *cobra.Command {
	var (
		env        []string
		name       string
		detach     bool
		rm         bool
		entrypoint string
		workdir    string
		memory     string
		volumes    []string
		network    string
		gpu        bool
		aliases    []string
		publish    []string
		publishAll bool
		expose     []string
		restart    string
	)
	cmd := &cobra.Command{
		Use:   "run [OPTIONS] IMAGE [COMMAND] [ARG...]",
		Short: "Create and run a new container from an image",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return &exitError{code: 125, err: err}
			}
			req := api.ContainerCreateRequest{}
			req.Image = args[0]
			req.Env = envFlags(env, lookup)
			req.WorkingDir = workdir
			req.HostConfig.AutoRemove = rm
			req.HostConfig.Binds = volumes
			req.HostConfig.NetworkMode = network
			if gpu {
				req.HostConfig.DeviceRequests = []api.DeviceRequest{api.GPURequest()}
			}
			if len(aliases) > 0 {
				if network == "" || network == "default" || network == "bridge" {
					return &exitError{code: 125, err: fmt.Errorf("--network-alias needs a user-defined --network")}
				}
				req.NetworkingConfig = &api.NetworkingConfig{EndpointsConfig: map[string]*api.EndpointSettings{network: {Aliases: aliases}}}
			}
			if err := portFlags(&req, publish, expose, publishAll); err != nil {
				return &exitError{code: 125, err: err}
			}
			if restart != "" {
				rp, err := parseRestart(restart)
				if err != nil {
					return &exitError{code: 125, err: err}
				}
				req.HostConfig.RestartPolicy = &rp
			}
			if len(args) > 1 {
				req.Cmd = args[1:]
			}
			if cmd.Flags().Changed("entrypoint") {
				req.Entrypoint = api.StrSlice{}
				if entrypoint != "" {
					req.Entrypoint = api.StrSlice{entrypoint}
				}
			}
			if memory != "" {
				n, err := units.RAMInBytes(memory)
				if err != nil {
					return &exitError{code: 125, err: fmt.Errorf("invalid --memory %q: %w", memory, err)}
				}
				req.HostConfig.Memory = n
			}
			return run(cmd, c, name, detach, rm, req)
		},
	}
	f := cmd.Flags()
	f.SetInterspersed(false) // flags after IMAGE belong to the container
	f.StringArrayVarP(&env, "env", "e", nil, "set environment variables (KEY=value, or KEY to pass this shell's value)")
	f.StringVar(&name, "name", "", "assign a name to the container")
	f.BoolVarP(&detach, "detach", "d", false, "run in the background and print the container ID")
	f.BoolVar(&rm, "rm", false, "remove the container when it exits")
	f.StringVar(&entrypoint, "entrypoint", "", "override the image's entrypoint")
	f.StringVarP(&workdir, "workdir", "w", "", "working directory inside the container")
	f.StringVarP(&memory, "memory", "m", "", "memory limit (e.g. 64m)")
	f.StringArrayVarP(&volumes, "volume", "v", nil, "mount a named volume: NAME:/path[:ro] (created if missing)")
	f.StringVar(&network, "network", "", "connect to a network (default bridge; none: no network)")
	f.BoolVar(&gpu, "gpu", false, "give the container the local GPU (wasi:webgpu, wasi-nn on the GPU)")
	f.StringArrayVar(&aliases, "network-alias", nil, "add a name the container answers to on --network (replicas may share one)")
	f.StringArrayVarP(&publish, "publish", "p", nil, "publish a port on the host: [ip:][hostPort:]containerPort[/tcp|udp] (ip defaults to 127.0.0.1)")
	f.BoolVarP(&publishAll, "publish-all", "P", false, "publish every exposed port on a host port the system picks")
	f.StringArrayVar(&expose, "expose", nil, "expose a port or a range without publishing it (80, 8000-8010/udp)")
	f.StringVar(&restart, "restart", "", "restart policy: no, always, unless-stopped, on-failure[:max-retries]")
	return cmd
}

// parseRestart reads --restart into the API's RestartPolicy; the daemon
// validates the name and the count.
func parseRestart(s string) (api.RestartPolicy, error) {
	name, n, hasN := strings.Cut(s, ":")
	rp := api.RestartPolicy{Name: name}
	if hasN {
		v, err := strconv.Atoi(n)
		if err != nil || v < 0 {
			return rp, fmt.Errorf("invalid --restart %q: the retry count is not a number", s)
		}
		rp.MaximumRetryCount = v
	}
	return rp, nil
}

func run(cmd *cobra.Command, c *api.Client, name string, detach, rm bool, req api.ContainerCreateRequest) error {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()
	created, err := c.Create(ctx, name, req)
	if api.IsNotFound(err) && strings.Contains(err.Error(), "no such image") {
		fmt.Fprintf(stderr, "Unable to find image '%s' locally\n", req.Image)
		if perr := pull(ctx, c, stderr, req.Image); perr != nil {
			return &exitError{code: 125, err: perr}
		}
		created, err = c.Create(ctx, name, req)
	}
	if err != nil {
		return &exitError{code: 125, err: err}
	}
	for _, w := range created.Warnings {
		fmt.Fprintln(stderr, "WARNING:", w)
	}
	if detach {
		if err := c.Start(ctx, created.ID); err != nil {
			return &exitError{code: 125, err: err}
		}
		fmt.Fprintln(stdout, created.ID)
		return nil
	}
	// Register the wait and attach the log stream before starting, so a
	// container that exits at once loses neither its output nor its code.
	cond := "next-exit"
	if rm {
		cond = "removed"
	}
	waiter, err := c.Wait(ctx, created.ID, cond)
	if err != nil {
		return &exitError{code: 125, err: err}
	}
	logs, err := c.Logs(ctx, created.ID, true)
	if err != nil {
		return &exitError{code: 125, err: err}
	}
	defer logs.Close()
	startErr := c.Start(ctx, created.ID)
	copied := make(chan error, 1)
	go func() { copied <- api.Demux(stdout, stderr, logs) }()
	if startErr != nil {
		logs.Close()
		<-copied
		// Docker's rule: the module could not be run (126) or found (127)
		// is the command's failure and keeps its code; any other start the
		// daemon refused (a volume, a network, a published port) is 125,
		// while the container records 128 with the reason.
		res, werr := waiter.Result()
		code := 125
		if werr == nil && (res.StatusCode == 126 || res.StatusCode == 127) {
			code = int(res.StatusCode)
		}
		return &exitError{code: code, err: startErr}
	}
	if err := <-copied; err != nil {
		return &exitError{code: 125, err: err}
	}
	res, err := waiter.Result()
	if err != nil {
		return &exitError{code: 125, err: err}
	}
	if res.StatusCode != 0 {
		return &exitError{code: int(res.StatusCode)}
	}
	return nil
}

func quoteCommand(s string) string {
	if len(s) > 20 {
		s = s[:19] + "…"
	}
	return `"` + s + `"`
}

func psCmd(client clientFunc) *cobra.Command {
	var all, quiet bool
	cmd := &cobra.Command{
		Use:   "ps",
		Short: "List containers",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			cs, err := c.Containers(cmd.Context(), all)
			if err != nil {
				return err
			}
			if quiet {
				for _, x := range cs {
					fmt.Fprintln(cmd.OutOrStdout(), x.ID[:12])
				}
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 3, ' ', 0)
			fmt.Fprintln(tw, "CONTAINER ID\tIMAGE\tCOMMAND\tCREATED\tSTATUS\tPORTS\tNAMES")
			for _, x := range cs {
				names := make([]string, len(x.Names))
				for i, n := range x.Names {
					names[i] = strings.TrimPrefix(n, "/")
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", x.ID[:12], x.Image, quoteCommand(x.Command),
					ago(time.Unix(x.Created, 0)), x.Status, formatPorts(x.Ports), strings.Join(names, ","))
			}
			return tw.Flush()
		},
	}
	cmd.Flags().BoolVarP(&all, "all", "a", false, "show all containers (default shows just running)")
	cmd.Flags().BoolVarP(&quiet, "quiet", "q", false, "only display container IDs")
	return cmd
}

func logsCmd(client clientFunc) *cobra.Command {
	var follow bool
	cmd := &cobra.Command{
		Use:   "logs CONTAINER",
		Short: "Fetch the logs of a container",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			rc, err := c.Logs(cmd.Context(), args[0], follow)
			if err != nil {
				return err
			}
			defer rc.Close()
			return api.Demux(cmd.OutOrStdout(), cmd.ErrOrStderr(), rc)
		},
	}
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "follow log output")
	return cmd
}

// each runs fn on every argument, printing what succeeds, reporting what
// fails, and failing at the end if anything did.
func each(cmd *cobra.Command, args []string, fn func(string) error) error {
	failed := 0
	for _, a := range args {
		if err := fn(a); err != nil {
			fmt.Fprintln(cmd.ErrOrStderr(), err)
			failed++
			continue
		}
	}
	if failed > 0 {
		return &exitError{code: 1}
	}
	return nil
}

func rmCmd(client clientFunc) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rm [OPTIONS] CONTAINER [CONTAINER...]",
		Short: "Remove one or more containers",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return each(cmd, args, func(id string) error {
				if err := c.Remove(cmd.Context(), id, force); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), id)
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "force the removal of a running container")
	return cmd
}

func rmiCmd(client clientFunc) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "rmi [OPTIONS] IMAGE [IMAGE...]",
		Short: "Remove one or more images",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return each(cmd, args, func(ref string) error {
				items, err := c.RemoveImage(cmd.Context(), ref, force)
				if err != nil {
					return err
				}
				for _, it := range items {
					if it.Untagged != "" {
						fmt.Fprintln(cmd.OutOrStdout(), "Untagged:", it.Untagged)
					}
					if it.Deleted != "" {
						fmt.Fprintln(cmd.OutOrStdout(), "Deleted:", it.Deleted)
					}
				}
				return nil
			})
		},
	}
	cmd.Flags().BoolVarP(&force, "force", "f", false, "force removal of an image in use")
	return cmd
}

func inspectCmd(client clientFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "inspect NAME|ID [NAME|ID...]",
		Short: "Show low-level information on containers or images",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			var out []json.RawMessage
			var failed error
			for _, a := range args {
				raw, err := c.Inspect(cmd.Context(), a)
				if api.IsNotFound(err) {
					raw, err = c.ImageInspect(cmd.Context(), a)
				}
				if err != nil {
					if api.IsNotFound(err) {
						err = fmt.Errorf("Error: No such object: %s", a)
					}
					fmt.Fprintln(cmd.ErrOrStderr(), err)
					failed = &exitError{code: 1}
					continue
				}
				out = append(out, raw)
			}
			if out == nil {
				out = []json.RawMessage{}
			}
			b, err := json.MarshalIndent(out, "", "    ")
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), string(b))
			return failed
		},
	}
}

func killCmd(client clientFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "kill CONTAINER [CONTAINER...]",
		Short: "Kill one or more running containers",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return each(cmd, args, func(id string) error {
				if err := c.Kill(cmd.Context(), id); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), id)
				return nil
			})
		},
	}
}

func startCmd(client clientFunc) *cobra.Command {
	var attach bool
	cmd := &cobra.Command{
		Use:   "start [OPTIONS] CONTAINER [CONTAINER...]",
		Short: "Start one or more stopped containers",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			if !attach {
				return each(cmd, args, func(id string) error {
					if err := c.Start(cmd.Context(), id); err != nil {
						return err
					}
					fmt.Fprintln(cmd.OutOrStdout(), id)
					return nil
				})
			}
			if len(args) > 1 {
				return &exitError{code: 1, err: errors.New("you cannot start and attach multiple containers at once")}
			}
			return startAttached(cmd.Context(), c, cmd.OutOrStdout(), cmd.ErrOrStderr(), args[0])
		},
	}
	cmd.Flags().BoolVarP(&attach, "attach", "a", false, "attach its output and exit with its status")
	return cmd
}

// startAttached is start -a: the wait and the output stream are taken
// before the start, as for run, so nothing is lost; the command exits with
// the container's status.
func startAttached(ctx context.Context, c *api.Client, stdout, stderr io.Writer, id string) error {
	waiter, err := c.Wait(ctx, id, "next-exit")
	if err != nil {
		return err
	}
	out, err := c.Attach(ctx, id)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := c.Start(ctx, id); err != nil {
		return err
	}
	if err := api.Demux(stdout, stderr, out); err != nil {
		return err
	}
	res, err := waiter.Result()
	if err != nil {
		return err
	}
	if res.StatusCode != 0 {
		return &exitError{code: int(res.StatusCode)}
	}
	return nil
}

func stopCmd(client clientFunc, restart bool) *cobra.Command {
	use, short := "stop", "Stop one or more running containers (a wasm guest has no signals: at once)"
	if restart {
		use, short = "restart", "Restart one or more containers"
	}
	var seconds int
	cmd := &cobra.Command{
		Use:   use + " [OPTIONS] CONTAINER [CONTAINER...]",
		Short: short,
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			return each(cmd, args, func(id string) error {
				op := c.Stop
				if restart {
					op = c.Restart
				}
				if err := op(cmd.Context(), id); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), id)
				return nil
			})
		},
	}
	// Accepted for compatibility: there is nothing to wait for, a wasm
	// guest cannot catch a signal.
	cmd.Flags().IntVarP(&seconds, "time", "t", 10, "seconds to wait before killing (ignored: wasm guests have no signals)")
	return cmd
}

func diffCmd(client clientFunc) *cobra.Command {
	return &cobra.Command{
		Use:   "diff CONTAINER",
		Short: "List the files a container added (A), changed (C) or deleted (D)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			changes, err := c.Changes(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			for _, ch := range changes {
				k := "?"
				if ch.Kind >= 0 && ch.Kind < 3 {
					k = string("CAD"[ch.Kind])
				}
				fmt.Fprintln(cmd.OutOrStdout(), k, ch.Path)
			}
			return nil
		},
	}
}

func versionCmd(client clientFunc, version string) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the client and daemon versions",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "Client:\n Version:     %s\n API version: %s\n", version, api.APIVersion)
			c, err := client()
			if err != nil {
				return err
			}
			v, err := c.Version(cmd.Context())
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "\nServer: %s\n Version:     %s\n API version: %s (minimum %s)\n Go version:  %s\n OS/Arch:     %s/%s\n",
				v.Platform.Name, v.Version, v.APIVersion, v.MinAPIVersion, v.GoVersion, v.Os, v.Arch)
			return nil
		},
	}
}
