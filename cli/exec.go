package cli

import (
	"context"
	"io"
	"os"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/go-wasmcontainers/engine/api"
)

// followSize sends the terminal's size to resize each time changes
// reports a possible change and the size differs from the last one sent
// (start: the size the exec was created with, [height, width]).
func followSize(ctx context.Context, get func() (w, h int, err error), resize func(h, w uint16) error,
	start []uint16, changes func(context.Context, func())) {
	var lastH, lastW uint16
	if len(start) == 2 {
		lastH, lastW = start[0], start[1]
	}
	var mu sync.Mutex
	changes(ctx, func() {
		w, h, err := get()
		if err != nil || w <= 0 || h <= 0 || w > 0xffff || h > 0xffff {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if uint16(h) == lastH && uint16(w) == lastW {
			return
		}
		if resize(uint16(h), uint16(w)) == nil {
			lastH, lastW = uint16(h), uint16(w)
		}
	})
}

// execCmd is `wctr exec [-d] [-i] [-t] [-e K=V] [-w DIR] [-u USER]
// CONTAINER CMD [ARG...]`: a further process in a running container.
func execCmd(client clientFunc, lookup func(string) (string, bool)) *cobra.Command {
	var (
		detach, interactive, tty bool
		env                      []string
		workdir, user            string
	)
	cmd := &cobra.Command{
		Use:   "exec [OPTIONS] CONTAINER COMMAND [ARG...]",
		Short: "Run a command in a running container",
		Long: "Run a command in a running container: a new process of the container, on its\n" +
			"filesystem and network, counted against its process limits. With -t it gets a\n" +
			"terminal (line editing and echo by the engine); with -i, this terminal's input.\n" +
			"The exit status is the command's.",
		Args: cobra.MinimumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}
			ctx := cmd.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			stdin, isFile := cmd.InOrStdin().(*os.File)
			rawTerm := tty && interactive && isFile && term.IsTerminal(int(stdin.Fd()))
			req := api.ExecCreateRequest{
				AttachStdin: interactive && !detach, AttachStdout: !detach, AttachStderr: !detach,
				Tty: tty, Env: envFlags(env, lookup), Cmd: args[1:], User: user, WorkingDir: workdir,
			}
			if out, ok := cmd.OutOrStdout().(*os.File); ok && tty && term.IsTerminal(int(out.Fd())) {
				if w, h, err := term.GetSize(int(out.Fd())); err == nil && w > 0 && h > 0 {
					req.ConsoleSize = []uint16{uint16(h), uint16(w)}
				}
			}
			id, err := c.ExecCreate(ctx, args[0], req)
			if err != nil {
				return err
			}
			start := api.ExecStartRequest{Detach: detach, Tty: tty, ConsoleSize: req.ConsoleSize}
			if detach {
				_, err := c.ExecStart(ctx, id, start, nil)
				return err
			}
			var in io.Reader
			if interactive {
				in = cmd.InOrStdin()
			}
			if rawTerm {
				// The engine does the line editing and the echo.
				old, err := term.MakeRaw(int(stdin.Fd()))
				if err == nil {
					defer term.Restore(int(stdin.Fd()), old)
				}
			}
			out, err := c.ExecStart(ctx, id, start, in)
			if err != nil {
				return err
			}
			defer out.Close()
			if f, ok := cmd.OutOrStdout().(*os.File); ok && tty && term.IsTerminal(int(f.Fd())) {
				// Follow the window: the guest's tty_get reports the new size.
				wctx, cancel := context.WithCancel(ctx)
				defer cancel()
				followSize(wctx, func() (int, int, error) { return term.GetSize(int(f.Fd())) },
					func(h, w uint16) error { return c.ExecResize(wctx, id, h, w) },
					req.ConsoleSize, sizeChanges)
			}
			if tty {
				_, err = io.Copy(cmd.OutOrStdout(), out)
			} else {
				err = api.Demux(cmd.OutOrStdout(), cmd.ErrOrStderr(), out)
			}
			if err != nil {
				return err
			}
			info, err := c.ExecInspect(context.WithoutCancel(ctx), id)
			if err != nil {
				return err
			}
			if info.ExitCode != nil && *info.ExitCode != 0 {
				return Exit(*info.ExitCode)
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.SetInterspersed(false) // flags after CONTAINER belong to the command
	f.BoolVarP(&detach, "detach", "d", false, "run the command in the background")
	f.BoolVarP(&interactive, "interactive", "i", false, "keep stdin open: send this terminal's input")
	f.BoolVarP(&tty, "tty", "t", false, "allocate a terminal")
	f.StringArrayVarP(&env, "env", "e", nil, "set an environment variable (KEY=VALUE, or KEY to pass this one)")
	f.StringVarP(&workdir, "workdir", "w", "", "working directory inside the container")
	f.StringVarP(&user, "user", "u", "", "user (WebAssembly containers have only root)")
	return cmd
}
