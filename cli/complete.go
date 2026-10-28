package cli

import (
	"context"
	"strings"
	"time"

	units "github.com/docker/go-units"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Shell completion, through cobra's hidden __complete command: the same
// logic serves `wctr completion bash|zsh|fish|powershell` natively and the
// playground's line editor in-process. Dynamic candidates come from the
// daemon (images, containers), bounded in number and time.

// maxCandidates bounds what one completion returns.
const maxCandidates = 200

// completionTimeout bounds the daemon queries of one completion.
const completionTimeout = 2 * time.Second

type completer struct {
	client clientFunc
	opt    Options
}

func filter(cands []cobra.Completion, toComplete string, skip map[string]bool) []cobra.Completion {
	out := make([]cobra.Completion, 0, len(cands))
	seen := map[string]bool{}
	for _, c := range cands {
		v, _, _ := strings.Cut(c, "\t")
		if !strings.HasPrefix(v, toComplete) || seen[v] || skip[v] {
			continue
		}
		seen[v] = true
		out = append(out, c)
		if len(out) == maxCandidates {
			break
		}
	}
	return out
}

func (c *completer) ctx(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	ctx := cmd.Context()
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, completionTimeout)
}

// images returns the local images as candidates: repo:tag, and the bare
// repository for a :latest tag (what `wctr run hello` accepts).
// networks are the engine's networks, with their driver.
func (c *completer) networks(cmd *cobra.Command) []cobra.Completion {
	cl, err := c.client()
	if err != nil {
		return nil
	}
	ctx, cancel := c.ctx(cmd)
	defer cancel()
	ns, err := cl.Networks(ctx)
	if err != nil {
		return nil
	}
	var out []cobra.Completion
	for _, n := range ns {
		out = append(out, cobra.CompletionWithDesc(n.Name, "network, "+n.Driver))
	}
	return out
}

func (c *completer) images(cmd *cobra.Command) []cobra.Completion {
	cl, err := c.client()
	if err != nil {
		return nil
	}
	ctx, cancel := c.ctx(cmd)
	defer cancel()
	imgs, err := cl.Images(ctx)
	if err != nil {
		return nil
	}
	var out []cobra.Completion
	for _, i := range imgs {
		size := units.HumanSizeWithPrecision(float64(i.Size), 3)
		for _, rt := range i.RepoTags {
			repo, tag := splitRepoTag(rt)
			if tag == "latest" {
				out = append(out, cobra.CompletionWithDesc(repo, "image, "+size))
			}
			out = append(out, cobra.CompletionWithDesc(rt, "image, "+size))
		}
	}
	return out
}

// catalog returns the images the host says can be pulled (the playground's
// mirror), marked as such.
func (c *completer) catalog(cmd *cobra.Command) []cobra.Completion {
	if c.opt.PullCandidates == nil {
		return nil
	}
	ctx, cancel := c.ctx(cmd)
	defer cancel()
	return c.opt.PullCandidates(ctx)
}

// containers returns container names (and, when the prefix looks like an
// ID, short IDs); running only unless all.
func (c *completer) containers(cmd *cobra.Command, all bool, toComplete string) []cobra.Completion {
	cl, err := c.client()
	if err != nil {
		return nil
	}
	ctx, cancel := c.ctx(cmd)
	defer cancel()
	cs, err := cl.Containers(ctx, all)
	if err != nil {
		return nil
	}
	var out []cobra.Completion
	for _, x := range cs {
		desc := x.Image + ", " + strings.ToLower(x.Status)
		for _, n := range x.Names {
			out = append(out, cobra.CompletionWithDesc(strings.TrimPrefix(n, "/"), desc))
		}
		if toComplete != "" && strings.HasPrefix(x.ID, toComplete) {
			out = append(out, cobra.CompletionWithDesc(x.ID[:12], desc))
		}
	}
	return out
}

const noFiles = cobra.ShellCompDirectiveNoFileComp

// Candidate is one completion choice.
type Candidate struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// Completion is what cobra's __complete answered.
type Completion struct {
	Candidates []Candidate
	Directive  cobra.ShellCompDirective
}

// NoSpace reports the NoSpace directive (no space after a unique match).
func (c Completion) NoSpace() bool { return c.Directive&cobra.ShellCompDirectiveNoSpace != 0 }

// Failed reports the Error directive (ignore the candidates).
func (c Completion) Failed() bool { return c.Directive&cobra.ShellCompDirectiveError != 0 }

// Complete runs the hidden __complete command in-process for words (the
// arguments after "wctr"; the last one is the word being completed, "" for
// a new word) and parses its output: one "value[\tdescription]" per line,
// then ":<directive>". This is what a shell's completion script does by
// running the binary; the playground's line editor does it here.
func Complete(ctx context.Context, o Options, words []string) (Completion, error) {
	if len(words) == 0 {
		words = []string{""}
	}
	cmd := RootCmd(o)
	var out, errb strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(append([]string{cobra.ShellCompRequestCmd}, words...))
	if err := cmd.ExecuteContext(ctx); err != nil {
		return Completion{}, err
	}
	var res Completion
	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, ":") && i == len(lines)-1 {
			var d int
			for _, ch := range l[1:] {
				if ch < '0' || ch > '9' {
					break
				}
				d = d*10 + int(ch-'0')
			}
			res.Directive = cobra.ShellCompDirective(d)
			continue
		}
		if l == "" {
			continue
		}
		v, desc, _ := strings.Cut(l, "\t")
		res.Candidates = append(res.Candidates, Candidate{Value: v, Description: desc})
		if len(res.Candidates) >= maxCandidates {
			break
		}
	}
	return res, nil
}

func used(args []string) map[string]bool {
	m := map[string]bool{}
	for _, a := range args {
		m[a] = true
	}
	return m
}

// install wires completion into the command tree.
func (c *completer) install(root *cobra.Command) {
	for _, sub := range root.Commands() {
		sub := sub
		switch sub.Name() {
		case "pull":
			sub.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
				if len(args) > 0 {
					return nil, noFiles
				}
				return filter(append(c.catalog(cmd), c.images(cmd)...), toComplete, nil), noFiles
			}
		case "run":
			sub.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
				if len(args) == 0 {
					// run pulls what it lacks: local images first, then the catalog.
					return filter(append(c.images(cmd), c.catalog(cmd)...), toComplete, nil), noFiles
				}
				if c.opt.RunArgs != nil {
					return filter(c.opt.RunArgs(args[0], args[1:]), toComplete, nil), noFiles
				}
				return nil, noFiles
			}
		case "rmi":
			sub.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
				return filter(c.images(cmd), toComplete, used(args)), noFiles
			}
		case "logs", "diff":
			sub.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
				if len(args) > 0 {
					return nil, noFiles
				}
				return filter(c.containers(cmd, true, toComplete), toComplete, nil), noFiles
			}
		case "rm", "start", "restart":
			sub.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
				return filter(c.containers(cmd, true, toComplete), toComplete, used(args)), noFiles
			}
		case "kill", "stop":
			sub.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
				return filter(c.containers(cmd, false, toComplete), toComplete, used(args)), noFiles
			}
		case "exec": // a running container, then its command
			sub.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
				if len(args) > 0 {
					return nil, noFiles
				}
				return filter(c.containers(cmd, false, toComplete), toComplete, nil), noFiles
			}
		case "inspect":
			sub.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
				return filter(append(c.containers(cmd, true, toComplete), c.images(cmd)...), toComplete, used(args)), noFiles
			}
		default:
			sub.ValidArgsFunction = cobra.NoFileCompletions
		}
		// No flag takes a file name; none has a value worth listing except
		// --memory, whose sizes are offered as examples.
		sub.Flags().VisitAll(func(f *pflag.Flag) {
			if f.Value.Type() == "bool" {
				return
			}
			if f.Name == "memory" {
				sub.RegisterFlagCompletionFunc(f.Name, cobra.FixedCompletions(
					[]cobra.Completion{"32m", "64m", "128m", "256m"}, noFiles))
				return
			}
			if f.Name == "network" {
				sub.RegisterFlagCompletionFunc(f.Name, func(cmd *cobra.Command, args []string, toComplete string) ([]cobra.Completion, cobra.ShellCompDirective) {
					return filter(c.networks(cmd), toComplete, nil), noFiles
				})
				return
			}
			sub.RegisterFlagCompletionFunc(f.Name, cobra.NoFileCompletions)
		})
	}
	root.RegisterFlagCompletionFunc("host", cobra.NoFileCompletions)
}
