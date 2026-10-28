package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/go-wasmcontainers/engine/api"
	"github.com/go-wasmcontainers/engine/internal/testutil"
)

// tarDir tars a directory (a layout written by testutil) with Go, not tar(1).
func tarDir(t *testing.T, dir string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel) + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: filepath.ToSlash(rel), Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(b))}); err != nil {
			return err
		}
		_, err = tw.Write(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	tw.Close()
	return buf.Bytes()
}

// TestBuilderAPI: what a builder needs from the engine, through the API
// client: load an OCI layout tar, copy files into a container and out,
// commit; and a command added to the tree from another module.
func TestBuilderAPI(t *testing.T) {
	ctx := context.Background()
	dir := shortDir(t)
	h, _ := daemonAt(t, dir)
	c, err := api.NewClient(h)
	if err != nil {
		t.Fatal(err)
	}
	layout := filepath.Join(shortDir(t), "layout")
	testutil.WriteLayout(t, layout, testutil.BuildContent(
		testutil.ImageSpec{RefName: "built/app:1", Module: testutil.Wasm(t, "probe"), ModulePath: "/bin/probe.wasm"},
	))
	lines, err := c.ImageLoad(ctx, bytes.NewReader(tarDir(t, layout)))
	if err != nil || len(lines) != 1 || !strings.Contains(lines[0], "Loaded image: built/app:1") {
		t.Fatalf("load: %q %v", lines, err)
	}
	must(t, wctr(t, h, "run", "--name", "b", "built/app:1", "exit", "0"), 0)

	// Copy a context into the container, then read it back.
	var ctxTar bytes.Buffer
	tw := tar.NewWriter(&ctxTar)
	for name, body := range map[string]string{"src/main.go": "package main", "README": "hi"} {
		tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(body))})
		tw.Write([]byte(body))
	}
	tw.Close()
	if err := c.CopyTo(ctx, "b", "/srv", bytes.NewReader(ctxTar.Bytes())); err == nil {
		t.Fatal("copied into a directory that does not exist")
	}
	if err := c.CopyTo(ctx, "b", "/", bytes.NewReader(ctxTar.Bytes())); err != nil {
		t.Fatal(err)
	}
	rc, err := c.CopyFrom(ctx, "b", "/src")
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(rc)
	var got []string
	for {
		hd, err := tr.Next()
		if err != nil {
			break
		}
		b, _ := io.ReadAll(tr)
		got = append(got, hd.Name+"="+string(b))
	}
	rc.Close()
	if strings.Join(got, " ") != "src/= src/main.go=package main" {
		t.Fatalf("copy out: %q", got)
	}
	// Hostile archive: refused with a 400, nothing written.
	var bad bytes.Buffer
	tw = tar.NewWriter(&bad)
	tw.WriteHeader(&tar.Header{Name: "../../etc/evil", Typeflag: tar.TypeReg, Size: 1})
	tw.Write([]byte("x"))
	tw.Close()
	var de *api.ErrDaemon
	if err := c.CopyTo(ctx, "b", "/", &bad); !errors.As(err, &de) || de.Status != http.StatusBadRequest {
		t.Fatalf("hostile archive: %v", err)
	}
	// The copied files survive a commit.
	if _, err := c.Commit(ctx, api.CommitOptions{Container: "b", Repo: "built/app", Tag: "2",
		Config: &api.ImageConfig{Cmd: api.StrSlice{"cat", "/src/main.go"}, Entrypoint: api.StrSlice{"/bin/probe.wasm"}, WorkingDir: "/"}}); err != nil {
		t.Fatal(err)
	}
	if r := must(t, wctr(t, h, "run", "--rm", "built/app:2"), 0); r.stdout != "package main" {
		t.Fatalf("committed context: %q", r.stdout)
	}
	// A layout tar that is not one: refused.
	if _, err := c.ImageLoad(ctx, bytes.NewReader(ctxTar.Bytes())); err == nil {
		t.Fatal("loaded a tar that is no OCI layout")
	}

	// Extra commands: a module adds `wctr hello-ext`, which reaches the
	// engine -H names.
	cmd := RootCmd(Options{Extra: []func(Engine) *cobra.Command{func(e Engine) *cobra.Command {
		return &cobra.Command{Use: "hello-ext", RunE: func(cmd *cobra.Command, _ []string) error {
			hc, base, err := e.HTTP()
			if err != nil {
				return err
			}
			resp, err := hc.Get(base + "/_ping")
			if err != nil {
				return err
			}
			defer resp.Body.Close()
			b, _ := io.ReadAll(resp.Body)
			cmd.Print("engine says " + string(b))
			return nil
		}}
	}}})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"-H", h, "hello-ext"})
	if err := cmd.Execute(); err != nil || out.String() != "engine says OK" {
		t.Fatalf("extra command: %q %v", out.String(), err)
	}
}
