package image_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/internal/testutil"
)

func documentSeeds() []any {
	c := testutil.BuildContent(
		testutil.ImageSpec{RefName: "a", Module: fakeModule},
		testutil.ImageSpec{RefName: "b", Module: fakeModule, Artifact: true, Index: true},
	)
	var out []any
	for _, b := range c.Blobs {
		if json.Valid(b) {
			out = append(out, b)
		}
	}
	out = append(out, []byte(`{"schemaVersion":2,"manifests":[{"mediaType":"a/b","digest":"sha256:`+strings.Repeat("0", 64)+`","size":-1}]}`),
		[]byte(`{"schemaVersion":2,"config":{"mediaType":"application/vnd.wasm.config.v0+json","digest":"sha256:`+strings.Repeat("1", 64)+`","size":1,"data":"eA=="},"layers":[]}`),
		[]byte(`[]`), []byte(`{"architecture":"wasm","os":"wasip1","config":{"Env":["A=1"]}}`))
	return out
}

var referenceSeeds = []any{"hello", "localhost:5000/a/b:c@sha256:" + strings.Repeat("a", 64), "[::1]:5000/x", "a//b", "UP/x:y", "oci:/tmp/l:tag"}

// FuzzParseDocuments throws arbitrary JSON at every document parser. They
// must not panic, and whatever they accept must satisfy the invariants the
// rest of the engine relies on: valid digests, non-negative sizes, bounded
// counts, and a single layer for a wasm artifact.
func FuzzParseDocuments(f *testing.F) {
	for _, s := range documentSeeds() {
		f.Add(s.([]byte))
	}
	lim := image.DefaultLimits
	lim.MaxLayers, lim.MaxIndexEntries = 8, 8
	f.Fuzz(func(t *testing.T, b []byte) {
		image.Sniff("", b)
		if m, err := image.ParseManifest(b, lim); err == nil {
			if len(m.Layers) > lim.MaxLayers {
				t.Fatal("too many layers accepted")
			}
			for _, d := range append([]image.Descriptor{m.Config}, m.Layers...) {
				if _, err := image.ParseDigest(string(d.Digest)); err != nil || d.Size < 0 {
					t.Fatalf("bad descriptor accepted: %+v", d)
				}
			}
			if m.Config.MediaType == image.MediaTypeWasmConfig && len(m.Layers) != 1 {
				t.Fatal("multi-layer wasm artifact accepted")
			}
		}
		if x, err := image.ParseIndex(b, lim); err == nil {
			if len(x.Manifests) > lim.MaxIndexEntries {
				t.Fatal("too many entries accepted")
			}
			image.SelectManifest(x, image.WasmPlatform)
		}
		if c, err := image.ParseConfig(b, lim); err == nil && (c.OS == "" || c.Architecture == "") {
			t.Fatal("config without platform accepted")
		}
	})
}

// FuzzReference: parsing never panics, and what parses re-parses to itself.
func FuzzReference(f *testing.F) {
	for _, s := range referenceSeeds {
		f.Add(s.(string))
	}
	f.Fuzz(func(t *testing.T, s string) {
		image.ParseChallenge(s)
		image.ParseLayoutRef(s)
		r, err := image.ParseReference(s)
		if err != nil {
			return
		}
		r2, err := image.ParseReference(r.String())
		if err != nil {
			// Familiar names may drop the domain; the canonical form must parse.
			t.Fatalf("%q -> %q does not re-parse: %v", s, r.String(), err)
		}
		if r2.String() != r.String() {
			t.Fatalf("%q -> %q -> %q", s, r.String(), r2.String())
		}
	})
}

func TestWriteFuzzCorpus(t *testing.T) {
	testutil.WriteCorpus(t, "FuzzParseDocuments", documentSeeds()...)
	testutil.WriteCorpus(t, "FuzzReference", referenceSeeds...)
}

func TestCorpusIsCommitted(t *testing.T) {
	for _, n := range []string{"FuzzParseDocuments", "FuzzReference"} {
		ents, err := os.ReadDir(filepath.Join("testdata", "fuzz", n))
		if err != nil || len(ents) == 0 {
			t.Fatalf("no committed corpus for %s: %v", n, err)
		}
	}
}
