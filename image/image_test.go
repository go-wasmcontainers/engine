package image_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/internal/testutil"
)

var fakeModule = []byte("\x00asm\x01\x00\x00\x00")

func TestDigest(t *testing.T) {
	d := image.FromBytes([]byte("abc"))
	if _, err := image.ParseDigest(string(d)); err != nil {
		t.Fatal(err)
	}
	if d.Algorithm() != "sha256" || len(d.Encoded()) != 64 || len(d.Short()) != 12 || d.String() != string(d) {
		t.Fatal(d)
	}
	for _, bad := range []string{"", "sha256", "md5:abc", "sha256:" + strings.Repeat("A", 64), "sha256:" + strings.Repeat("a", 63),
		"sha256:" + strings.Repeat("g", 64), "sha256:../../../../etc/passwd" + strings.Repeat("a", 36)} {
		if _, err := image.ParseDigest(bad); !errors.Is(err, image.ErrDigest) {
			t.Errorf("%q accepted", bad)
		}
	}
	if d.Verify([]byte("abd")) == nil || d.Verify([]byte("abc")) != nil {
		t.Fatal("verify")
	}
	if image.Digest("sha256:ab").Short() != "ab" {
		t.Fatal("short")
	}
}

func TestVerifyingReader(t *testing.T) {
	b := []byte("hello world")
	d := image.FromBytes(b)
	got, err := io.ReadAll(image.NewVerifyingReader(bytes.NewReader(b), d, int64(len(b))))
	if err != nil || !bytes.Equal(got, b) {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(image.NewVerifyingReader(bytes.NewReader(b), d, 5)); !errors.Is(err, image.ErrTooLarge) {
		t.Fatal("long stream:", err)
	}
	if _, err := io.ReadAll(image.NewVerifyingReader(bytes.NewReader(b), d, 20)); !errors.Is(err, image.ErrDigest) {
		t.Fatal("short stream:", err)
	}
	other := image.FromBytes([]byte("hello worle"))
	if _, err := io.ReadAll(image.NewVerifyingReader(bytes.NewReader(b), other, int64(len(b)))); !errors.Is(err, image.ErrDigest) {
		t.Fatal("wrong digest:", err)
	}
}

func TestReference(t *testing.T) {
	cases := []struct{ in, name, str, host, object string }{
		{"hello", "hello", "hello:latest", image.DockerHubRegistry, "latest"},
		{"docker.io/library/hello:1", "hello", "hello:1", image.DockerHubRegistry, "1"},
		{"index.docker.io/u/app", "u/app", "u/app:latest", image.DockerHubRegistry, "latest"},
		{"u/app:v2", "u/app", "u/app:v2", image.DockerHubRegistry, "v2"},
		{"localhost:5000/x/y:t", "localhost:5000/x/y", "localhost:5000/x/y:t", "localhost:5000", "t"},
		{"localhost/x", "localhost/x", "localhost/x:latest", "localhost", "latest"},
		{"ghcr.io/o/r@sha256:" + strings.Repeat("a", 64), "ghcr.io/o/r", "ghcr.io/o/r@sha256:" + strings.Repeat("a", 64), "ghcr.io", "sha256:" + strings.Repeat("a", 64)},
		{"Registry/x", "Registry/x", "Registry/x:latest", "Registry", "latest"},
	}
	for _, c := range cases {
		r, err := image.ParseReference(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if r.Name() != c.name || r.String() != c.str || r.RegistryHost() != c.host || r.Object() != c.object {
			t.Errorf("%s: name=%s str=%s host=%s object=%s", c.in, r.Name(), r.String(), r.RegistryHost(), r.Object())
		}
	}
	for _, bad := range []string{"", "UPPER", "a:b:c", "x/Y", "x@sha256:zz", "x:" + strings.Repeat("t", 129), "bad host!/x", "x/../y", strings.Repeat("a", 600)} {
		if _, err := image.ParseReference(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if n, _ := image.NormalizeName("docker.io/library/hello"); n != "hello:latest" {
		t.Fatal(n)
	}
	if _, err := image.NormalizeName("!"); err == nil {
		t.Fatal("bad name accepted")
	}
}

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TestParseDocuments(t *testing.T) {
	lim := image.DefaultLimits
	cfg := image.Descriptor{MediaType: image.MediaTypeOCIConfig, Digest: image.FromBytes([]byte("{}")), Size: 2}
	good := image.Manifest{SchemaVersion: 2, MediaType: image.MediaTypeOCIManifest, Config: cfg, Layers: []image.Descriptor{}}
	if _, err := image.ParseManifest(mustMarshal(good), lim); err != nil {
		t.Fatal(err)
	}
	// Unknown fields are not an error (manifest.md / config.md).
	if _, err := image.ParseManifest([]byte(`{"schemaVersion":2,"config":`+string(mustMarshal(cfg))+`,"layers":[],"future":1}`), lim); err != nil {
		t.Fatal(err)
	}
	bad := map[string]func(m *image.Manifest){
		"schema 1":      func(m *image.Manifest) { m.SchemaVersion = 1 },
		"media type":    func(m *image.Manifest) { m.MediaType = image.MediaTypeOCIIndex },
		"config digest": func(m *image.Manifest) { m.Config.Digest = "sha256:x" },
		"negative size": func(m *image.Manifest) { m.Config.Size = -1 },
		"bad mediatype": func(m *image.Manifest) { m.Config.MediaType = "no slash" },
		"data mismatch": func(m *image.Manifest) { m.Config.Data = []byte("{ }"); m.Config.Size = 3 },
		"data size":     func(m *image.Manifest) { m.Config.Data = []byte("{}"); m.Config.Size = 5 },
		"wasm two layers": func(m *image.Manifest) {
			m.Config.MediaType = image.MediaTypeWasmConfig
			m.Layers = []image.Descriptor{cfg, cfg}
		},
		"layer digest": func(m *image.Manifest) { m.Layers = []image.Descriptor{{MediaType: "a/b", Digest: "x"}} },
	}
	for name, mut := range bad {
		m := good
		m.Config = cfg
		mut(&m)
		if _, err := image.ParseManifest(mustMarshal(m), lim); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// Embedded data that matches is fine.
	m := good
	m.Config.Data = []byte("{}")
	if _, err := image.ParseManifest(mustMarshal(m), lim); err != nil {
		t.Fatal(err)
	}
	for _, junk := range []string{"", "[]", "{} {}", "{", `{"schemaVersion":"2"}`} {
		if _, err := image.ParseManifest([]byte(junk), lim); err == nil {
			t.Errorf("manifest %q accepted", junk)
		}
		if _, err := image.ParseIndex([]byte(junk), lim); err == nil {
			t.Errorf("index %q accepted", junk)
		}
		if _, err := image.ParseConfig([]byte(junk), lim); err == nil {
			t.Errorf("config %q accepted", junk)
		}
	}
	small := lim
	small.MaxManifestBytes, small.MaxConfigBytes, small.MaxLayers, small.MaxIndexEntries = 10, 10, 0, 0
	if _, err := image.ParseManifest(mustMarshal(good), small); !errors.Is(err, image.ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := image.ParseIndex(mustMarshal(good), small); !errors.Is(err, image.ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := image.ParseConfig([]byte(`{"architecture":"wasm","os":"wasip1"}`), small); !errors.Is(err, image.ErrTooLarge) {
		t.Fatal(err)
	}
	small.MaxManifestBytes = lim.MaxManifestBytes
	lm := good
	lm.Layers = []image.Descriptor{cfg}
	if _, err := image.ParseManifest(mustMarshal(lm), small); !errors.Is(err, image.ErrTooLarge) {
		t.Fatal(err)
	}
	x := image.Index{SchemaVersion: 2, Manifests: []image.Descriptor{cfg}}
	if _, err := image.ParseIndex(mustMarshal(x), small); !errors.Is(err, image.ErrTooLarge) {
		t.Fatal(err)
	}
	for name, raw := range map[string]string{
		"no manifests": `{"schemaVersion":2}`,
		"schema":       `{"schemaVersion":1,"manifests":[]}`,
		"media type":   `{"schemaVersion":2,"mediaType":"x/y","manifests":[]}`,
		"bad entry":    `{"schemaVersion":2,"manifests":[{"mediaType":"a/b","digest":"no","size":1}]}`,
	} {
		if _, err := image.ParseIndex([]byte(raw), lim); err == nil {
			t.Errorf("index %s accepted", name)
		}
	}
	if _, err := image.ParseConfig([]byte(`{"os":"wasip1"}`), lim); err == nil {
		t.Fatal("config without architecture accepted")
	}
}

func TestSniffAndSelect(t *testing.T) {
	cases := map[string]image.Kind{
		image.MediaTypeOCIManifest + "; charset=utf-8": image.KindManifest,
		image.MediaTypeDockerList:                      image.KindIndex,
	}
	for mt, want := range cases {
		if image.Sniff(mt, nil) != want {
			t.Fatal(mt)
		}
	}
	for doc, want := range map[string]image.Kind{
		`{"mediaType":"` + image.MediaTypeOCIIndex + `"}`:       image.KindIndex,
		`{"mediaType":"` + image.MediaTypeDockerManifest + `"}`: image.KindManifest,
		`{"manifests":[]}`: image.KindIndex,
		`{"layers":[]}`:    image.KindManifest,
		`{}`:               image.KindUnknown,
		`nope`:             image.KindUnknown,
	} {
		if got := image.Sniff("", []byte(doc)); got != want {
			t.Errorf("%s: %v", doc, got)
		}
	}
	d := func(os, arch string) image.Descriptor {
		return image.Descriptor{MediaType: image.MediaTypeOCIManifest, Digest: image.FromBytes([]byte(os + arch)), Size: 1,
			Platform: &image.Platform{OS: os, Architecture: arch}}
	}
	x := &image.Index{Manifests: []image.Descriptor{
		{MediaType: "application/vnd.example.attestation", Digest: image.FromBytes(nil)},
		{MediaType: image.MediaTypeOCIManifest, Digest: image.FromBytes([]byte("np"))},
		d("linux", "amd64"), d("wasip1", "wasm"),
	}}
	got, err := image.SelectManifest(x, image.WasmPlatform)
	if err != nil || got.Platform.OS != "wasip1" {
		t.Fatal(got, err)
	}
	_, err = image.SelectManifest(&image.Index{Manifests: []image.Descriptor{d("linux", "arm64")}}, image.WasmPlatform)
	if !errors.Is(err, image.ErrNoPlatform) || !strings.Contains(err.Error(), "linux/arm64") {
		t.Fatal(err)
	}
	_, err = image.SelectManifest(&image.Index{}, image.WasmPlatform)
	if !errors.Is(err, image.ErrNoPlatform) {
		t.Fatal(err)
	}
	if (image.Platform{OS: "linux", Architecture: "arm64", Variant: "v8"}).String() != "linux/arm64/v8" {
		t.Fatal("platform string")
	}
}

func layoutDir(t *testing.T, c *testutil.Content) string {
	dir := filepath.Join(t.TempDir(), "layout")
	testutil.WriteLayout(t, dir, c)
	return dir
}

func TestPullFromLayout(t *testing.T) {
	c := testutil.BuildContent(
		testutil.ImageSpec{RefName: "hello", Module: fakeModule},
		testutil.ImageSpec{RefName: "art", Module: fakeModule, Artifact: true, Index: true},
		testutil.ImageSpec{RefName: "linux", Module: fakeModule, OS: "linux"},
	)
	dir := layoutDir(t, c)
	storeDir := filepath.Join(t.TempDir(), "store")
	st, err := image.OpenStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var lines []string
	progress := func(id, s string) { lines = append(lines, id+" "+s) }
	for _, ref := range []string{"hello", "art"} {
		src, name, err := image.OpenSource("oci:"+dir+":"+ref, image.RegistryOptions{}, image.DefaultLimits)
		if err != nil {
			t.Fatal(err)
		}
		if name != ref+":latest" {
			t.Fatal(name)
		}
		res, err := image.Pull(ctx, st, src, image.DefaultLimits, progress)
		if err != nil {
			t.Fatalf("%s: %v", ref, err)
		}
		if !res.Downloaded || res.Image.Config.OS != "wasip1" {
			t.Fatal(res)
		}
		if ref == "art" && (!res.Image.Wasm || res.RepoDigest == res.Image.ManifestDigest) {
			t.Fatal("artifact behind an index")
		}
		if err := st.Tag(name, image.Ref{Manifest: res.Image.ManifestDigest, RepoDigest: res.RepoDigest}); err != nil {
			t.Fatal(err)
		}
		// A second pull downloads nothing.
		res2, err := image.Pull(ctx, st, src, image.DefaultLimits, progress)
		if err != nil || res2.Downloaded {
			t.Fatal("second pull downloaded:", err)
		}
	}
	if !strings.Contains(strings.Join(lines, "\n"), "Already exists") {
		t.Fatal(lines)
	}
	// A linux image is refused.
	src, _, _ := image.OpenSource("oci:"+dir+":linux", image.RegistryOptions{}, image.DefaultLimits)
	if _, err := image.Pull(ctx, st, src, image.DefaultLimits, nil); err == nil || !strings.Contains(err.Error(), "wasip1/wasm") {
		t.Fatal(err)
	}
	// Names survive a reopen; the image graph is readable from disk.
	st, err = image.OpenStore(storeDir)
	if err != nil {
		t.Fatal(err)
	}
	names := st.Names()
	if len(names) != 2 || names[0].Name != "art:latest" {
		t.Fatal(names)
	}
	img, err := st.Image(names[1].Manifest, image.DefaultLimits)
	if err != nil || img.ID() != img.ConfigDigest || img.Size <= 0 {
		t.Fatal(img, err)
	}
	// GC drops only the manifest and config the refused linux pull left,
	// keeps everything named, then drops what an untag orphaned.
	if gone, err := st.GC(image.DefaultLimits); err != nil || len(gone) != 2 {
		t.Fatal(gone, err)
	}
	if gone, err := st.GC(image.DefaultLimits); err != nil || len(gone) != 0 {
		t.Fatal(gone, err)
	}
	if ok, err := st.Untag("hello:latest"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if ok, _ := st.Untag("hello:latest"); ok {
		t.Fatal("double untag")
	}
	gone, err := st.GC(image.DefaultLimits)
	if err != nil || len(gone) == 0 {
		t.Fatal(gone, err)
	}
	if _, ok := st.Resolve("hello:latest"); ok {
		t.Fatal("resolve after untag")
	}
	if _, ok := st.Resolve("art:latest"); !ok {
		t.Fatal("art lost")
	}
}

func TestLayoutErrors(t *testing.T) {
	c := testutil.BuildContent(testutil.ImageSpec{RefName: "a", Module: fakeModule}, testutil.ImageSpec{RefName: "b", Module: fakeModule})
	dir := layoutDir(t, c)
	ctx := context.Background()
	if _, err := image.ParseLayoutRef("oci:"); err == nil {
		t.Fatal("empty layout ref")
	}
	if _, err := image.ParseLayoutRef("oci::tag"); err == nil {
		t.Fatal("empty dir")
	}
	if r, _ := image.ParseLayoutRef("oci:/x/y"); r.Dir != "/x/y" || r.RefName != "" {
		t.Fatal(r)
	}
	if n, _ := (image.LayoutRef{Dir: "/tmp/My-App/"}).LocalName(); n != "my-app:latest" {
		t.Fatal(n)
	}
	// Two entries and no name: ambiguous.
	src, _, err := image.OpenSource("oci:"+dir, image.RegistryOptions{}, image.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := src.Resolve(ctx); err == nil {
		t.Fatal("ambiguous layout resolved")
	}
	src, _, _ = image.OpenSource("oci:"+dir+":zzz", image.RegistryOptions{}, image.DefaultLimits)
	if _, err := src.Resolve(ctx); !errors.Is(err, image.ErrNotFound) {
		t.Fatal(err)
	}
	if _, _, err := image.OpenSource("oci:"+filepath.Join(dir, "nope")+":a", image.RegistryOptions{}, image.DefaultLimits); err == nil {
		t.Fatal("missing dir")
	}
	if _, _, err := image.OpenSource("oci:"+filepath.Join(dir, "index.json")+":a", image.RegistryOptions{}, image.DefaultLimits); err == nil {
		t.Fatal("file as dir")
	}
	if _, _, err := image.OpenSource("oci:"+t.TempDir()+":a", image.RegistryOptions{}, image.DefaultLimits); err == nil {
		t.Fatal("no oci-layout")
	}
	bad := t.TempDir()
	os.WriteFile(filepath.Join(bad, "oci-layout"), []byte(`{}`), 0o644)
	if _, _, err := image.OpenSource("oci:"+bad+":a", image.RegistryOptions{}, image.DefaultLimits); err == nil {
		t.Fatal("oci-layout without version")
	}
	os.WriteFile(filepath.Join(bad, "oci-layout"), []byte(`nope`), 0o644)
	if _, _, err := image.OpenSource("oci:"+bad+":a", image.RegistryOptions{}, image.DefaultLimits); err == nil {
		t.Fatal("oci-layout not JSON")
	}
	// A blob that does not match its name is refused at pull.
	for d, b := range c.Blobs {
		if bytes.HasPrefix(b, []byte{0x1f, 0x8b}) {
			os.WriteFile(filepath.Join(dir, "blobs", "sha256", d.Encoded()), append(b, 0), 0o644)
		}
	}
	st, _ := image.OpenStore(t.TempDir())
	src, _, _ = image.OpenSource("oci:"+dir+":a", image.RegistryOptions{}, image.DefaultLimits)
	if _, err := image.Pull(ctx, st, src, image.DefaultLimits, nil); !errors.Is(err, image.ErrTooLarge) && !errors.Is(err, image.ErrDigest) {
		t.Fatal("tampered blob accepted:", err)
	}
	// Remove a blob: not found.
	for d, b := range c.Blobs {
		if bytes.HasPrefix(b, []byte{0x1f, 0x8b}) {
			os.Remove(filepath.Join(dir, "blobs", "sha256", d.Encoded()))
		}
	}
	if _, err := image.Pull(ctx, st, src, image.DefaultLimits, nil); !errors.Is(err, image.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := src.Fetch(ctx, image.Descriptor{Digest: "sha256:../../etc"}); !errors.Is(err, image.ErrDigest) {
		t.Fatal(err)
	}
}

func TestPullLimits(t *testing.T) {
	c := testutil.BuildContent(testutil.ImageSpec{RefName: "a", Module: fakeModule})
	dir := layoutDir(t, c)
	ctx := context.Background()
	for name, mut := range map[string]func(*image.Limits){
		"blob":     func(l *image.Limits) { l.MaxBlobBytes = 10 },
		"image":    func(l *image.Limits) { l.MaxImageBytes = 10 },
		"config":   func(l *image.Limits) { l.MaxConfigBytes = 10 },
		"manifest": func(l *image.Limits) { l.MaxManifestBytes = 10 },
	} {
		lim := image.DefaultLimits
		mut(&lim)
		st, _ := image.OpenStore(t.TempDir())
		src, _, _ := image.OpenSource("oci:"+dir+":a", image.RegistryOptions{}, image.DefaultLimits)
		if _, err := image.Pull(ctx, st, src, lim, nil); !errors.Is(err, image.ErrTooLarge) {
			t.Errorf("%s limit: %v", name, err)
		}
	}
}

func TestStoreErrors(t *testing.T) {
	root := t.TempDir()
	st, err := image.OpenStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if st.Has("nope") {
		t.Fatal("has invalid")
	}
	if _, err := st.Open("nope"); err == nil {
		t.Fatal("open invalid")
	}
	if _, err := st.Open(image.FromBytes([]byte("x"))); !errors.Is(err, image.ErrNotFound) {
		t.Fatal(err)
	}
	if err := st.Ingest(image.Descriptor{Digest: "bad"}, nil); err == nil {
		t.Fatal("ingest invalid")
	}
	b := []byte("blob")
	d := image.Descriptor{Digest: image.FromBytes(b), Size: 4}
	if err := st.Ingest(d, bytes.NewReader([]byte("blub"))); !errors.Is(err, image.ErrDigest) {
		t.Fatal(err)
	}
	if st.Has(d.Digest) {
		t.Fatal("a failed ingest left the blob")
	}
	if err := st.Ingest(d, bytes.NewReader(b)); err != nil {
		t.Fatal(err)
	}
	if err := st.Ingest(d, nil); err != nil {
		t.Fatal("re-ingest:", err)
	}
	if _, err := st.ReadBlob(d.Digest, 2); !errors.Is(err, image.ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := st.Image(d.Digest, image.DefaultLimits); err == nil {
		t.Fatal("a blob that is not a manifest made an image")
	}
	// A dangling name makes GC refuse rather than delete on a partial view.
	st.Tag("x:latest", image.Ref{Manifest: image.FromBytes([]byte("missing"))})
	if _, err := st.GC(image.DefaultLimits); err == nil {
		t.Fatal("gc on a broken graph")
	}
	// Corrupt refs.json is reported.
	os.WriteFile(filepath.Join(root, "refs.json"), []byte("{"), 0o600)
	if _, err := image.OpenStore(root); err == nil {
		t.Fatal("corrupt refs.json accepted")
	}
	os.WriteFile(filepath.Join(root, "refs.json"), []byte(`{"x":{"manifest":"bad"}}`), 0o600)
	if _, err := image.OpenStore(root); err == nil {
		t.Fatal("bad digest in refs.json accepted")
	}
}
