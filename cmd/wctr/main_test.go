package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMainExitStatus(t *testing.T) {
	code := -1
	osExit = func(c int) { code = c }
	defer func() { osExit = os.Exit }()
	os.Args = []string{"wctr", "-H", "unix:///nonexistent/x.sock", "ps"}
	main()
	if code != 1 {
		t.Fatal(code)
	}
}

func run(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	cmd := rootCmd()
	var out, errb bytes.Buffer
	cmd.SetArgs(args)
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

// TestBuildAndCompose: both are wctr subcommands, and with TraverseChildren
// a subcommand's short flags are its own: compose's -f is --file (a
// project read without a daemon), logs' -f is --follow (reaching the
// daemon, which is not there), -H before the subcommand still names it.
func TestBuildAndCompose(t *testing.T) {
	for _, name := range []string{"build", "compose"} {
		if c, _, err := rootCmd().Find([]string{name}); err != nil || c.Name() != name {
			t.Fatalf("no %s command: %v", name, err)
		}
	}
	dir := t.TempDir()
	f := filepath.Join(dir, "app.yaml")
	if err := os.WriteFile(f, []byte("services:\n  web:\n    image: hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := run(t, "-H", "unix:///nonexistent/x.sock", "compose", "-f", f, "config")
	if err != nil || !strings.Contains(out, "web") {
		t.Fatalf("compose -f FILE config: %v\nstdout: %s\nstderr: %s", err, out, stderr)
	}
	_, stderr, err = run(t, "-H", "unix:///nonexistent/x.sock", "logs", "-f", "web")
	if err == nil || strings.Contains(err.Error(), "unknown shorthand") || strings.Contains(stderr, "unknown shorthand") {
		t.Fatalf("logs -f: %v %s", err, stderr)
	}
	if !strings.Contains(err.Error(), "x.sock") && !strings.Contains(err.Error(), "daemon") {
		t.Fatalf("logs -f did not try the daemon named by -H: %v", err)
	}
}
