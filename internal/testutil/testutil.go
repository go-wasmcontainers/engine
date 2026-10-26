// Package testutil builds what the engine's tests run: wasip1 programs
// compiled from testdata at test time, OCI image content made from them
// (both the CNCF wasm artifact form and ordinary tar-layer images), OCI
// image layouts on disk, and an in-process distribution registry.
package testutil

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/go-wasmcontainers/engine/image"
)

var (
	wasmMu    sync.Mutex
	wasmCache = map[string][]byte{}
)

// RepoRoot is the module root, found from this file's location.
func RepoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// Wasm compiles testdata/wasm/<name> for wasip1/wasm once per process.
func Wasm(tb testing.TB, name string) []byte { return WasmTags(tb, name, "") }

// WasmTags is Wasm with build tags (comma-separated).
func WasmTags(tb testing.TB, name, tags string) []byte {
	tb.Helper()
	wasmMu.Lock()
	defer wasmMu.Unlock()
	key := name + "|" + tags
	if b, ok := wasmCache[key]; ok {
		return b
	}
	dir, err := os.MkdirTemp("", "wasmbuild-")
	if err != nil {
		tb.Fatal(err)
	}
	defer os.RemoveAll(dir)
	out := filepath.Join(dir, name+".wasm")
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-tags="+tags, "-o", out, "./testdata/wasm/"+name)
	cmd.Dir = RepoRoot()
	// A guest with its own go.mod (its dependencies are not the engine's)
	// builds in its directory.
	if src := filepath.Join(RepoRoot(), "testdata", "wasm", name); fileExists(filepath.Join(src, "go.mod")) {
		cmd = exec.Command("go", "build", "-trimpath", "-ldflags=-s -w", "-tags="+tags, "-o", out, ".")
		cmd.Dir = src
	}
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm", "CGO_ENABLED=0", "GOWORK=off")
	cmd.Stdin = nil
	if msg, err := cmd.CombinedOutput(); err != nil {
		tb.Fatalf("building %s for wasip1: %v\n%s", name, err, msg)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		tb.Fatal(err)
	}
	wasmCache[key] = b
	return b
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// TarEntry is one entry of a test tar.
type TarEntry struct {
	Name string
	Type byte // tar.TypeReg when zero
	Body []byte
	Link string
	Mode int64
}

// Tar builds an uncompressed tar archive.
func Tar(entries ...TarEntry) []byte {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, e := range entries {
		h := &tar.Header{Name: e.Name, Typeflag: e.Type, Linkname: e.Link, Mode: e.Mode, ModTime: time.Unix(1700000000, 0)}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Mode == 0 {
			h.Mode = 0o644
			if h.Typeflag == tar.TypeDir {
				h.Mode = 0o755
			}
		}
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(h); err != nil {
			panic(err)
		}
		if h.Typeflag == tar.TypeReg {
			tw.Write(e.Body)
		}
	}
	tw.Close()
	return buf.Bytes()
}

// Gzip compresses b.
func Gzip(b []byte) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	zw.Write(b)
	zw.Close()
	return buf.Bytes()
}

// ImageSpec describes one test image.
type ImageSpec struct {
	// RefName is the org.opencontainers.image.ref.name / registry tag.
	RefName string
	// Module is the wasm binary. In artifact form it is the single layer;
	// otherwise it is placed at ModulePath inside a tar+gzip layer.
	Module []byte
	// Artifact selects the CNCF wasm artifact form.
	Artifact bool
	// ModulePath is where the module goes in a tar image (default
	// /bin/app.wasm); it becomes the entrypoint.
	ModulePath string
	// Files are extra files of the tar image's first layer.
	Files map[string]string
	// Layers are extra raw layers (uncompressed tar), applied after.
	Layers [][]byte
	Cmd    []string
	Env    []string
	// WorkingDir of the config.
	WorkingDir string
	// Index wraps the manifest in an index that also lists a linux/amd64
	// manifest, so selection is exercised.
	Index bool
	// OS overrides the config's os (default wasip1).
	OS string
}

// Content is a set of blobs and named roots, servable by a layout or a
// registry.
type Content struct {
	Blobs map[image.Digest][]byte
	// Roots maps a ref name to the descriptor it names.
	Roots map[string]image.Descriptor
}

func (c *Content) add(mt string, b []byte) image.Descriptor {
	d := image.FromBytes(b)
	c.Blobs[d] = b
	return image.Descriptor{MediaType: mt, Digest: d, Size: int64(len(b))}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// BuildContent makes the blobs of the given images.
func BuildContent(specs ...ImageSpec) *Content {
	c := &Content{Blobs: map[image.Digest][]byte{}, Roots: map[string]image.Descriptor{}}
	for _, s := range specs {
		c.Roots[s.RefName] = c.addImage(s)
	}
	return c
}

func (c *Content) addImage(s ImageSpec) image.Descriptor {
	osName := s.OS
	if osName == "" {
		osName = "wasip1"
	}
	var m image.Manifest
	m.SchemaVersion = 2
	m.MediaType = image.MediaTypeOCIManifest
	if s.Artifact {
		layer := c.add(image.MediaTypeWasmLayer, s.Module)
		layer.Annotations = map[string]string{image.AnnotationTitle: "app.wasm"}
		cfg := map[string]any{
			"created": "2026-10-26T00:00:00Z", "architecture": "wasm", "os": osName,
			"layerDigests": []string{string(layer.Digest)},
		}
		m.Config = c.add(image.MediaTypeWasmConfig, mustJSON(cfg))
		m.Layers = []image.Descriptor{layer}
	} else {
		mp := s.ModulePath
		if mp == "" {
			mp = "/bin/app.wasm"
		}
		entries := []TarEntry{{Name: "bin/", Type: tar.TypeDir}, {Name: mp[1:], Body: s.Module, Mode: 0o755}}
		names := make([]string, 0, len(s.Files))
		for n := range s.Files {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			entries = append(entries, TarEntry{Name: n, Body: []byte(s.Files[n])})
		}
		raw := [][]byte{Tar(entries...)}
		raw = append(raw, s.Layers...)
		var diffIDs []string
		for i, l := range raw {
			diffIDs = append(diffIDs, string(image.FromBytes(l)))
			if i == 0 {
				m.Layers = append(m.Layers, c.add(image.MediaTypeOCILayerGz, Gzip(l)))
			} else {
				m.Layers = append(m.Layers, c.add(image.MediaTypeOCILayer, l))
			}
		}
		cfg := image.Config{
			Created: "2026-10-26T00:00:00Z", Architecture: "wasm", OS: osName,
			Config: image.ExecConfig{Entrypoint: []string{mp}, Cmd: s.Cmd, Env: s.Env, WorkingDir: s.WorkingDir},
			RootFS: image.RootFS{Type: "layers", DiffIDs: diffIDs},
		}
		m.Config = c.add(image.MediaTypeOCIConfig, mustJSON(cfg))
	}
	md := c.add(image.MediaTypeOCIManifest, mustJSON(m))
	if !s.Index {
		return md
	}
	// A decoy linux/amd64 manifest the engine must skip.
	other := image.Manifest{SchemaVersion: 2, MediaType: image.MediaTypeOCIManifest,
		Config: c.add(image.MediaTypeOCIConfig, mustJSON(image.Config{Architecture: "amd64", OS: "linux", RootFS: image.RootFS{Type: "layers"}})),
		Layers: []image.Descriptor{}}
	od := c.add(image.MediaTypeOCIManifest, mustJSON(other))
	od.Platform = &image.Platform{OS: "linux", Architecture: "amd64"}
	md.Platform = &image.Platform{OS: osName, Architecture: "wasm"}
	x := image.Index{SchemaVersion: 2, MediaType: image.MediaTypeOCIIndex, Manifests: []image.Descriptor{od, md}}
	return c.add(image.MediaTypeOCIIndex, mustJSON(x))
}

// WriteLayout writes c as an OCI image layout in dir.
func WriteLayout(tb testing.TB, dir string, c *Content) {
	tb.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "blobs", "sha256"), 0o755); err != nil {
		tb.Fatal(err)
	}
	for d, b := range c.Blobs {
		if err := os.WriteFile(filepath.Join(dir, "blobs", "sha256", d.Encoded()), b, 0o644); err != nil {
			tb.Fatal(err)
		}
	}
	names := make([]string, 0, len(c.Roots))
	for n := range c.Roots {
		names = append(names, n)
	}
	sort.Strings(names)
	x := image.Index{SchemaVersion: 2, MediaType: image.MediaTypeOCIIndex, Manifests: []image.Descriptor{}}
	for _, n := range names {
		d := c.Roots[n]
		d.Annotations = map[string]string{image.AnnotationRefName: n}
		x.Manifests = append(x.Manifests, d)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.json"), mustJSON(x), 0o644); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644); err != nil {
		tb.Fatal(err)
	}
}

// Must panics on error; for table setup.
func Must[T any](v T, err error) T {
	if err != nil {
		panic(fmt.Sprint(err))
	}
	return v
}
