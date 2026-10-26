package testutil

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/go-wasmcontainers/engine/image"
)

// Registry is a minimal in-process OCI distribution registry serving one
// Content under one repository, read-only. It implements exactly the pull
// half of the distribution spec plus the Docker token flow, which is all
// the engine speaks; go-containerregistry's registry has no token auth, and
// a test of the token flow needs a server that demands one.
type Registry struct {
	*httptest.Server
	Repo    string
	content *Content

	// RequireToken makes every /v2 request demand a Bearer token from /token.
	RequireToken bool
	// LieDigest makes manifest responses carry a wrong Docker-Content-Digest.
	LieDigest bool
	// Corrupt flips a byte of every blob served.
	Corrupt bool

	mu       sync.Mutex
	Requests []string
	Tokens   atomic.Int32
}

const testToken = "test-token-0123456789"

// NewRegistry starts a registry; call Close when done.
func NewRegistry(repo string, c *Content) *Registry {
	r := &Registry{Repo: repo, content: c}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	return r
}

// Host is the registry's host:port.
func (r *Registry) Host() string { return strings.TrimPrefix(r.URL, "http://") }

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"errors": []map[string]string{{"code": code, "message": msg}}})
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	r.Requests = append(r.Requests, req.Method+" "+req.URL.Path)
	r.mu.Unlock()
	if req.URL.Path == "/token" {
		r.Tokens.Add(1)
		if req.URL.Query().Get("scope") != "repository:"+r.Repo+":pull" {
			http.Error(w, "bad scope", http.StatusForbidden)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"token": testToken, "expires_in": 300})
		return
	}
	if !strings.HasPrefix(req.URL.Path, "/v2/") {
		http.NotFound(w, req)
		return
	}
	if r.RequireToken && req.Header.Get("Authorization") != "Bearer "+testToken {
		w.Header().Set("Www-Authenticate", `Bearer realm="`+r.URL+`/token",service="test-registry",scope="repository:`+r.Repo+`:pull"`)
		writeErr(w, http.StatusUnauthorized, "UNAUTHORIZED", "authentication required")
		return
	}
	rest := strings.TrimPrefix(req.URL.Path, "/v2/")
	if rest == "" {
		w.WriteHeader(http.StatusOK)
		return
	}
	prefix := r.Repo + "/"
	if !strings.HasPrefix(rest, prefix) {
		writeErr(w, http.StatusNotFound, "NAME_UNKNOWN", "repository name not known to registry")
		return
	}
	kind, object, ok := strings.Cut(strings.TrimPrefix(rest, prefix), "/")
	if !ok {
		http.NotFound(w, req)
		return
	}
	switch kind {
	case "manifests":
		var d image.Descriptor
		if root, ok := r.content.Roots[object]; ok {
			d = root
		} else if b, ok := r.content.Blobs[image.Digest(object)]; ok {
			d = image.Descriptor{Digest: image.Digest(object), Size: int64(len(b)), MediaType: sniffType(b)}
		} else {
			writeErr(w, http.StatusNotFound, "MANIFEST_UNKNOWN", "manifest unknown")
			return
		}
		b := r.content.Blobs[d.Digest]
		w.Header().Set("Content-Type", d.MediaType)
		dg := string(d.Digest)
		if r.LieDigest {
			dg = string(image.FromBytes([]byte("lie")))
		}
		w.Header().Set("Docker-Content-Digest", dg)
		w.Write(b)
	case "blobs":
		b, ok := r.content.Blobs[image.Digest(object)]
		if !ok {
			writeErr(w, http.StatusNotFound, "BLOB_UNKNOWN", "blob unknown to registry")
			return
		}
		if r.Corrupt && len(b) > 0 {
			b = append([]byte(nil), b...)
			b[len(b)/2] ^= 0xff
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Write(b)
	default:
		http.NotFound(w, req)
	}
}

func sniffType(b []byte) string {
	if image.Sniff("", b) == image.KindIndex {
		return image.MediaTypeOCIIndex
	}
	return image.MediaTypeOCIManifest
}
