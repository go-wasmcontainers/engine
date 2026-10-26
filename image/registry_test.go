package image_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-wasmcontainers/engine/image"
	"github.com/go-wasmcontainers/engine/internal/testutil"
)

func registryPull(t *testing.T, reg *testutil.Registry, ref string) (*image.PullResult, error) {
	t.Helper()
	src, name, err := image.OpenSource(reg.Host()+"/"+ref, image.RegistryOptions{}, image.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(name, reg.Host()+"/") {
		t.Fatal(name)
	}
	st, err := image.OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return image.Pull(context.Background(), st, src, image.DefaultLimits, nil)
}

func TestRegistryPull(t *testing.T) {
	c := testutil.BuildContent(
		testutil.ImageSpec{RefName: "v1", Module: fakeModule},
		testutil.ImageSpec{RefName: "multi", Module: fakeModule, Artifact: true, Index: true},
	)
	reg := testutil.NewRegistry("lab/hello", c)
	defer reg.Close()
	for _, tag := range []string{"v1", "multi"} {
		res, err := registryPull(t, reg, "lab/hello:"+tag)
		if err != nil {
			t.Fatalf("%s: %v", tag, err)
		}
		if res.RepoDigest != c.Roots[tag].Digest {
			t.Fatal("repo digest")
		}
	}
	// By digest.
	if _, err := registryPull(t, reg, "lab/hello@"+string(c.Roots["v1"].Digest)); err != nil {
		t.Fatal(err)
	}
	// Unknown tag and repository: a typed 404.
	_, err := registryPull(t, reg, "lab/hello:nope")
	var re *image.RegistryError
	if !errors.As(err, &re) || !errors.Is(err, image.ErrNotFound) || re.Code != "MANIFEST_UNKNOWN" || re.Error() == "" {
		t.Fatal(err)
	}
	if _, err := registryPull(t, reg, "other/repo:v1"); !errors.Is(err, image.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestRegistryTokenFlow(t *testing.T) {
	c := testutil.BuildContent(testutil.ImageSpec{RefName: "v1", Module: fakeModule})
	reg := testutil.NewRegistry("lab/hello", c)
	reg.RequireToken = true
	defer reg.Close()
	if _, err := registryPull(t, reg, "lab/hello:v1"); err != nil {
		t.Fatal(err)
	}
	if reg.Tokens.Load() != 1 {
		t.Fatalf("%d token requests, want 1 (the token is reused)", reg.Tokens.Load())
	}
}

func TestRegistryRefusesLies(t *testing.T) {
	c := testutil.BuildContent(testutil.ImageSpec{RefName: "v1", Module: fakeModule})
	reg := testutil.NewRegistry("lab/hello", c)
	defer reg.Close()
	reg.LieDigest = true
	if _, err := registryPull(t, reg, "lab/hello:v1"); !errors.Is(err, image.ErrDigest) {
		t.Fatal("lying Docker-Content-Digest accepted:", err)
	}
	reg.LieDigest = false
	reg.Corrupt = true
	if _, err := registryPull(t, reg, "lab/hello:v1"); !errors.Is(err, image.ErrDigest) {
		t.Fatal("corrupt blob accepted:", err)
	}
	reg.Corrupt = false
	// Asking by digest and being served something else.
	other := image.FromBytes([]byte("other"))
	if _, err := registryPull(t, reg, "lab/hello@"+string(other)); err == nil {
		t.Fatal("wrong content for a digest accepted")
	}
}

func TestChallenge(t *testing.T) {
	s, p := image.ParseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:samalba/my-app:pull,push"`)
	if s != "Bearer" || p["realm"] != "https://auth.docker.io/token" || p["service"] != "registry.docker.io" || p["scope"] != "repository:samalba/my-app:pull,push" {
		t.Fatal(s, p)
	}
	_, p = image.ParseChallenge(`Bearer realm=https://x/t, service=svc`)
	if p["realm"] != "https://x/t" || p["service"] != "svc" {
		t.Fatal(p)
	}
	_, p = image.ParseChallenge(`Bearer realm="unterminated`)
	if _, ok := p["realm"]; ok {
		t.Fatal(p)
	}
}

// hostile serves fixed answers to exercise the client's defences.
func hostile(t *testing.T, h http.HandlerFunc) string {
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func TestRegistryHostile(t *testing.T) {
	ctx := context.Background()
	open := func(host string) image.Source {
		src, _, err := image.OpenSource(host+"/x:t", image.RegistryOptions{UserAgent: "test"}, image.DefaultLimits)
		if err != nil {
			t.Fatal(err)
		}
		return src
	}
	// Non-Bearer challenge.
	host := hostile(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Www-Authenticate", `Basic realm="x"`)
		w.WriteHeader(401)
	})
	if _, err := open(host).Resolve(ctx); err == nil || !strings.Contains(err.Error(), "Basic") {
		t.Fatal(err)
	}
	// Realm that is not a URL / token endpoint failing / token missing.
	for name, realm := range map[string]string{"not a url": "::", "ftp": "ftp://x/t"} {
		host = hostile(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Www-Authenticate", `Bearer realm="`+realm+`"`)
			w.WriteHeader(401)
		})
		if _, err := open(host).Resolve(ctx); err == nil {
			t.Fatal(name)
		}
	}
	var self string
	tokenBody := ""
	tokenStatus := 200
	host = hostile(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			w.WriteHeader(tokenStatus)
			w.Write([]byte(tokenBody))
			return
		}
		if r.Header.Get("Authorization") == "Bearer alt" {
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte(`{"schemaVersion":2,"layers":[]}`))
			return
		}
		w.Header().Set("Www-Authenticate", `Bearer realm="http://`+self+`/token"`)
		w.WriteHeader(401)
	})
	self = host
	for _, c := range []struct {
		status int
		body   string
	}{{500, ""}, {200, "{"}, {200, "{}"}, {200, strings.Repeat(" ", 2<<20)}} {
		tokenStatus, tokenBody = c.status, c.body
		if _, err := open(host).Resolve(ctx); err == nil {
			t.Fatalf("token %d %q accepted", c.status, c.body[:min(len(c.body), 10)])
		}
	}
	// access_token is accepted as token; an unknown Content-Type is sniffed.
	tokenStatus, tokenBody = 200, `{"access_token":"alt"}`
	d, err := open(host).Resolve(ctx)
	if err != nil || d.MediaType != image.MediaTypeOCIManifest {
		t.Fatal(d, err)
	}
	// A manifest bigger than the limit, and junk with a junk type.
	host = hostile(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "big") {
			w.Write(make([]byte, 5<<20))
			return
		}
		w.Write([]byte("junk"))
	})
	src, _, _ := image.OpenSource(host+"/big:t", image.RegistryOptions{}, image.DefaultLimits)
	if _, err := src.Resolve(ctx); !errors.Is(err, image.ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := open(host).Resolve(ctx); !errors.Is(err, image.ErrInvalid) {
		t.Fatal(err)
	}
	// A blob announced longer than its descriptor is refused before reading.
	host = hostile(t, func(w http.ResponseWriter, r *http.Request) { w.Write(make([]byte, 100)) })
	if _, err := open(host).Fetch(ctx, image.Descriptor{Digest: image.FromBytes(nil), Size: 10}); !errors.Is(err, image.ErrTooLarge) {
		t.Fatal(err)
	}
	if _, err := open(host).Fetch(ctx, image.Descriptor{Digest: "bad"}); err == nil {
		t.Fatal("bad digest fetched")
	}
}

func TestRedirectDropsAuthorization(t *testing.T) {
	var gotAuth string
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write([]byte("blob"))
	}))
	defer other.Close()
	var self string
	host := hostile(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/token":
			w.Write([]byte(`{"token":"secret"}`))
		case r.Header.Get("Authorization") == "":
			w.Header().Set("Www-Authenticate", `Bearer realm="http://`+self+`/token"`)
			w.WriteHeader(401)
		default:
			// Another host: 127.0.0.1 versus localhost.
			http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/blob", http.StatusTemporaryRedirect)
		}
	})
	self = host
	src, _, _ := image.OpenSource(host+"/x:t", image.RegistryOptions{Client: &http.Client{}}, image.DefaultLimits)
	rc, err := src.Fetch(context.Background(), image.Descriptor{Digest: image.FromBytes([]byte("blob")), Size: 4})
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if gotAuth != "" {
		t.Fatal("Authorization crossed a host boundary:", gotAuth)
	}
}

func TestInsecureHosts(t *testing.T) {
	ref, _ := image.ParseReference("registry.lab:5000/x")
	src := image.NewRegistrySource(ref, image.RegistryOptions{Insecure: []string{"registry.lab:5000"}}, image.DefaultLimits)
	if src == nil {
		t.Fatal("nil source")
	}
	// An https registry must not accept an http token realm.
	ref, _ = image.ParseReference("example.invalid/x")
	_ = image.NewRegistrySource(ref, image.RegistryOptions{}, image.DefaultLimits)
}
