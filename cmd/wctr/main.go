// Command wctr is the engine's client. Its subcommands follow the familiar
// container CLI grammar (pull, run, ps, logs, rm, rmi, inspect, images),
// and it talks to wctrd over a unix socket with an API compatible with the
// Docker Engine API.
//
// It imports no runtime and nothing native, so it also builds for
// GOOS=wasip1 (it will run inside the browser sandbox).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"text/tabwriter"
	"time"

	units "github.com/docker/go-units"
	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/image"
)

// osExit is replaced by tests.
var osExit = os.Exit

// version is set at build time with -ldflags "-X main.version=…".
var version = "dev"

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
		osExit(report(cmd.ErrOrStderr(), err))
	}
}

// report prints err and returns the exit status.
func report(w io.Writer, err error) int {
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
func RootCmd() *cobra.Command {
	var host string
	root := &cobra.Command{
		Use:           "wctr",
		Short:         "Run WebAssembly (wasip1) containers with the go-wasmcontainers engine",
		SilenceUsage:  true,
		SilenceErrors: true,
		Version:       version,
	}
	root.PersistentFlags().StringVarP(&host, "host", "H", api.DefaultHost(), "daemon socket to connect to (unix:///path; default $"+api.HostEnv+", else unix://"+api.DefaultSocket+")")
	client := func() (*api.Client, error) { return api.NewClient(host) }

	root.AddCommand(
		pullCmd(client), imagesCmd(client), runCmd(client), psCmd(client), logsCmd(client),
		rmCmd(client), rmiCmd(client), inspectCmd(client), killCmd(client), versionCmd(client),
	)
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
func envFlags(in []string) []string {
	var out []string
	for _, e := range in {
		if strings.Contains(e, "=") {
			out = append(out, e)
		} else if v, ok := os.LookupEnv(e); ok {
			out = append(out, e+"="+v)
		}
	}
	return out
}

func runCmd(client clientFunc) *cobra.Command {
	var (
		env        []string
		name       string
		detach     bool
		rm         bool
		entrypoint string
		workdir    string
		memory     string
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
			req.Env = envFlags(env)
			req.WorkingDir = workdir
			req.HostConfig.AutoRemove = rm
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
	return cmd
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
		res, werr := waiter.Result()
		code := 125
		if werr == nil && res.StatusCode != 0 {
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
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t\t%s\n", x.ID[:12], x.Image, quoteCommand(x.Command),
					ago(time.Unix(x.Created, 0)), x.Status, strings.Join(names, ","))
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

func versionCmd(client clientFunc) *cobra.Command {
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
