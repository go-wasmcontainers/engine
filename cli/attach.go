package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/go-wasmcontainers/engine/api"
)

// session is an attached container: run -it, start -ai, attach.
type session struct {
	stdin      io.Reader // nil: no input
	tty        bool
	detachKeys string
	// start starts the container once attached (run, start -a); cond is
	// the wait condition for its exit status.
	start bool
	cond  string
	// startErr, when set, reports a refused start (run maps it to 125).
	onStartErr func(error) error
}

// attachSession attaches to container id, starts it if asked, and copies
// its output until it stops or the user detaches. It returns the
// container's exit status, or detached.
func attachSession(cmd *cobra.Command, c *api.Client, id string, s session) (code int, detached bool, err error) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel() // ends the request: its body stops reading our stdin
	stdout, stderr := cmd.OutOrStdout(), cmd.ErrOrStderr()

	waiter, err := c.Wait(ctx, id, s.cond)
	if err != nil {
		return 0, false, err
	}
	in := s.stdin
	if f, ok := in.(*os.File); ok && s.tty && term.IsTerminal(int(f.Fd())) {
		// The engine is the line discipline: this terminal goes raw.
		if old, err := term.MakeRaw(int(f.Fd())); err == nil {
			defer term.Restore(int(f.Fd()), old)
		}
	}
	out, err := c.AttachWith(ctx, id, api.AttachOptions{Stdin: in, DetachKeys: s.detachKeys})
	if err != nil {
		return 0, false, err
	}
	defer out.Close()
	if s.start {
		if err := c.Start(ctx, id); err != nil {
			out.Close()
			if s.onStartErr != nil {
				return 0, false, s.onStartErr(err)
			}
			return 0, false, err
		}
	}
	if f, ok := stdout.(*os.File); ok && s.tty && term.IsTerminal(int(f.Fd())) {
		followSize(ctx, func() (int, int, error) { return term.GetSize(int(f.Fd())) },
			func(h, w uint16) error { return c.ContainerResize(ctx, id, h, w) }, nil, sizeChanges)
	}
	if s.tty {
		_, err = io.Copy(stdout, out)
	} else {
		err = api.Demux(stdout, stderr, out)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		return 0, false, err
	}
	// The stream ended: the container stopped, or the user detached and
	// it runs on.
	if running(context.WithoutCancel(ctx), c, id) {
		return 0, true, nil
	}
	res, err := waiter.Result()
	if err != nil {
		return 0, false, err
	}
	return int(res.StatusCode), false, nil
}

// running reports whether container id is running now.
func running(ctx context.Context, c *api.Client, id string) bool {
	raw, err := c.Inspect(ctx, id)
	if err != nil {
		return false
	}
	var v struct {
		State struct{ Running bool }
	}
	return json.Unmarshal(raw, &v) == nil && v.State.Running
}

// attachCmd is `wctr attach [--no-stdin] [--detach-keys K] CONTAINER`.
func attachCmd(client clientFunc) *cobra.Command {
	var noStdin bool
	var keys string
	cmd := &cobra.Command{
		Use:   "attach [OPTIONS] CONTAINER",
		Short: "Attach this terminal to a running container's input and output",
		Long: "Attach this terminal to a running container: its output, and its input if it was\n" +
			"created with -i. Ctrl-P Ctrl-Q (or --detach-keys) detaches and leaves it running.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			cfg, err := containerIO(cmd.Context(), c, args[0])
			if err != nil {
				return err
			}
			if !cfg.Running {
				return fmt.Errorf("you cannot attach to a stopped container, start it first")
			}
			var in io.Reader
			if cfg.OpenStdin && !noStdin {
				in = cmd.InOrStdin()
			}
			code, _, err := attachSession(cmd, c, args[0], session{stdin: in, tty: cfg.Tty, detachKeys: keys, cond: "not-running"})
			if err != nil {
				return err
			}
			if code != 0 {
				return Exit(code)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&noStdin, "no-stdin", false, "do not attach stdin")
	cmd.Flags().StringVar(&keys, "detach-keys", "", "the key sequence that detaches (default ctrl-p,ctrl-q)")
	return cmd
}

type ioConfig struct {
	Running, Tty, OpenStdin bool
}

func containerIO(ctx context.Context, c *api.Client, id string) (ioConfig, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	raw, err := c.Inspect(ctx, id)
	if err != nil {
		return ioConfig{}, err
	}
	var v struct {
		State  struct{ Running bool }
		Config struct{ Tty, OpenStdin bool }
	}
	if err := json.Unmarshal(raw, &v); err != nil {
		return ioConfig{}, err
	}
	return ioConfig{Running: v.State.Running, Tty: v.Config.Tty, OpenStdin: v.Config.OpenStdin}, nil
}
