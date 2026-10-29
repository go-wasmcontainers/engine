package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/container"
)

func values(c Completion) string {
	var v []string
	for _, x := range c.Candidates {
		v = append(v, x.Value)
	}
	return strings.Join(v, ",")
}

func TestCompletion(t *testing.T) {
	host := startDaemon(t, container.Options{})
	layout := testLayout(t)
	must(t, wctr(t, host, "pull", "oci:"+layout+":hello"), 0)
	must(t, wctr(t, host, "pull", "oci:"+layout+":probe"), 0)
	must(t, wctr(t, host, "run", "--name", "done", "hello"), 0)
	must(t, wctr(t, host, "run", "-d", "--name", "spinning", "probe", "spin"), 0)
	defer wctr(t, host, "rm", "-f", "spinning")

	o := Options{
		DefaultHost: host,
		PullCandidates: func(context.Context) []cobra.Completion {
			return []cobra.Completion{"lab\tfrom the mirror", "hello\tfrom the mirror"}
		},
		RunArgs: func(image string, args []string) []cobra.Completion {
			if image == "probe" && len(args) == 0 {
				return []cobra.Completion{"exit\tends", "echo\tprints", "spin\tloops"}
			}
			return nil
		},
	}
	comp := func(words ...string) Completion {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		t0 := time.Now()
		c, err := Complete(ctx, o, words)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("complete %q in %v: %s", words, time.Since(t0), values(c))
		return c
	}
	for _, tc := range []struct {
		words []string
		want  string
	}{
		{[]string{""}, "commit,completion,diff,events,exec,help,images,inspect,kill,logs,network,port,ps,pull,restart,rm,rmi,run,start,stats,stop,system,tag,version,volume"},
		{[]string{"system", ""}, "df,prune"},
		{[]string{"r"}, "restart,rm,rmi,run"},
		{[]string{"ru"}, "run"},
		{[]string{"run", "--rm", ""}, "hello,hello:latest,probe,probe:latest,lab"},
		{[]string{"run", "pr"}, "probe,probe:latest"},
		{[]string{"run", "probe", ""}, "exit,echo,spin"},
		{[]string{"run", "probe", "e"}, "exit,echo"},
		{[]string{"run", "hello", ""}, ""},
		{[]string{"run", "-m", ""}, "32m,64m,128m,256m"},
		{[]string{"run", "--name", ""}, ""},
		{[]string{"run", "--network", ""}, "bridge,none"},
		{[]string{"run", "--network", "b"}, "bridge"},
		{[]string{"pull", ""}, "lab,hello,hello:latest,probe,probe:latest"},
		{[]string{"pull", "hello", ""}, ""},
		{[]string{"rmi", "hello", ""}, "hello:latest,probe,probe:latest"},
		{[]string{"logs", ""}, "spinning,done"},
		{[]string{"logs", "done", ""}, ""},
		{[]string{"kill", ""}, "spinning"},
		{[]string{"rm", "spinning", ""}, "done"},
		// "do", not "d": a random hex ID may begin with d, and would
		// rightly be offered too.
		{[]string{"inspect", "do"}, "done"},
		{[]string{"ps", ""}, ""},
	} {
		c := comp(tc.words...)
		if got := values(c); got != tc.want {
			// Order is the daemon's for containers; compare as sets there.
			if !sameSet(got, tc.want) {
				t.Errorf("%q: %q, want %q", tc.words, got, tc.want)
			}
		}
		if c.Failed() {
			t.Errorf("%q: error directive", tc.words)
		}
	}
	// Flags complete by prefix, with their usage as description.
	c := comp("run", "--en")
	if values(c) != "--entrypoint,--env" || c.Candidates[1].Description == "" {
		t.Fatalf("%+v", c)
	}
	// Descriptions say what a candidate is.
	c = comp("logs", "")
	for _, x := range c.Candidates {
		if !strings.Contains(x.Description, "probe") && !strings.Contains(x.Description, "hello") {
			t.Fatalf("%+v", x)
		}
	}
	// A short ID completes when the prefix is one.
	ps := wctr(t, host, "ps", "-q")
	id := strings.TrimSpace(ps.stdout)
	if c = comp("kill", id[:4]); values(c) != id {
		t.Fatalf("%q vs %q", values(c), id)
	}
	// An unreachable daemon gives no candidates, not an error.
	o.DefaultHost = "unix:///nonexistent/x.sock"
	if c = comp("logs", ""); len(c.Candidates) != 0 || c.Failed() {
		t.Fatalf("%+v", c)
	}
	if c = comp(); len(c.Candidates) == 0 {
		t.Fatal("no words: subcommands expected")
	}
	if !(Completion{Directive: cobra.ShellCompDirectiveNoSpace}).NoSpace() {
		t.Fatal("NoSpace")
	}
	// The browser has no shell to give scripts to.
	o.NoCompletionCommand = true
	if c = comp("comp"); len(c.Candidates) != 0 {
		t.Fatalf("%+v", c)
	}
	// Natively, the scripts are there.
	if r := wctr(t, host, "completion", "bash"); r.code != 0 || !strings.Contains(r.stdout, "__complete") {
		t.Fatal(r.code)
	}
}

func sameSet(a, b string) bool {
	as, bs := strings.Split(a, ","), strings.Split(b, ",")
	if len(as) != len(bs) {
		return false
	}
	m := map[string]int{}
	for _, x := range as {
		m[x]++
	}
	for _, x := range bs {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}
