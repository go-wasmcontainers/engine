package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// manifestAccept is sent on every manifest request: the distribution spec
// says the client SHOULD list the manifest types it supports.
var manifestAccept = strings.Join([]string{
	MediaTypeOCIIndex, MediaTypeOCIManifest, MediaTypeDockerList, MediaTypeDockerManifest,
}, ", ")

// RegistryOptions configures a registry source.
type RegistryOptions struct {
	// Client is the HTTP client; nil means a default one. Its CheckRedirect
	// is replaced so Authorization never crosses hosts.
	Client *http.Client
	// Insecure lists registry hosts reached over plain HTTP. Loopback hosts
	// (localhost, 127.0.0.0/8, ::1) are always plain HTTP, as Docker does.
	Insecure []string
	// UserAgent is sent on every request.
	UserAgent string
	// Mirrors maps a reference's domain ("docker.io" for familiar names)
	// to the base URL that serves its /v2 API, which may carry a path
	// prefix: "https://example.org/playground" makes "hello" resolve to
	// https://example.org/playground/v2/library/hello/manifests/latest.
	Mirrors map[string]string
}

// RegistrySource pulls one reference from an OCI distribution registry.
type RegistrySource struct {
	ref    Reference
	base   string // scheme://host
	client *http.Client
	ua     string
	lim    Limits

	mu    sync.Mutex
	token string
}

// NewRegistrySource prepares a pull of ref.
func NewRegistrySource(ref Reference, opt RegistryOptions, lim Limits) *RegistrySource {
	host := ref.RegistryHost()
	scheme := "https"
	if isInsecure(host, opt.Insecure) {
		scheme = "http"
	}
	c := &http.Client{}
	if opt.Client != nil {
		cc := *opt.Client
		c = &cc
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		// MUST NOT forward Authorization across host boundaries.
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		return nil
	}
	ua := opt.UserAgent
	if ua == "" {
		ua = "go-wasmcontainers-engine"
	}
	base := scheme + "://" + host
	if m, ok := opt.Mirrors[ref.Domain]; ok {
		base = strings.TrimSuffix(m, "/")
	}
	return &RegistrySource{ref: ref, base: base, client: c, ua: ua, lim: lim}
}

func isInsecure(host string, list []string) bool {
	for _, h := range list {
		if h == host {
			return true
		}
	}
	h := host
	if hh, _, err := net.SplitHostPort(host); err == nil {
		h = hh
	}
	h = strings.Trim(h, "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// RegistryError is a non-success answer from a registry.
type RegistryError struct {
	Status int
	Code   string
	Msg    string
}

func (e *RegistryError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("registry: %d %s: %s", e.Status, e.Code, e.Msg)
	}
	return fmt.Sprintf("registry: %d %s", e.Status, http.StatusText(e.Status))
}

// Is lets errors.Is(err, ErrNotFound) match a 404.
func (e *RegistryError) Is(target error) bool {
	return target == ErrNotFound && e.Status == http.StatusNotFound
}

func registryError(resp *http.Response) error {
	e := &RegistryError{Status: resp.StatusCode}
	b, _ := readAllLimited(resp.Body, 64<<10)
	var body struct {
		Errors []struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	if json.Unmarshal(b, &body) == nil && len(body.Errors) > 0 {
		e.Code, e.Msg = body.Errors[0].Code, body.Errors[0].Message
	}
	return e
}

// do sends a GET, answering one Bearer challenge if the registry asks.
func (r *RegistrySource) do(ctx context.Context, u, accept string) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", r.ua)
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		r.mu.Lock()
		tok := r.token
		r.mu.Unlock()
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := r.client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusUnauthorized || attempt > 0 {
			return resp, nil
		}
		ch := resp.Header.Get("Www-Authenticate")
		resp.Body.Close()
		if err := r.authenticate(ctx, ch); err != nil {
			return nil, err
		}
	}
}

// ParseChallenge parses `Bearer realm="…",service="…",scope="…"`.
func ParseChallenge(h string) (scheme string, params map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params = map[string]string{}
	for len(rest) > 0 {
		rest = strings.TrimLeft(rest, " ,")
		k, v, ok := strings.Cut(rest, "=")
		if !ok {
			break
		}
		k = strings.ToLower(strings.TrimSpace(k))
		if strings.HasPrefix(v, `"`) {
			end := strings.IndexByte(v[1:], '"')
			if end < 0 {
				break
			}
			params[k] = v[1 : 1+end]
			rest = v[2+end:]
		} else {
			val, next, _ := strings.Cut(v, ",")
			params[k] = strings.TrimSpace(val)
			rest = next
		}
	}
	return scheme, params
}

func (r *RegistrySource) authenticate(ctx context.Context, challenge string) error {
	scheme, p := ParseChallenge(challenge)
	if !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("registry requires %q authentication, which is not supported", scheme)
	}
	realm, err := url.Parse(p["realm"])
	if err != nil || realm.Host == "" || (realm.Scheme != "https" && realm.Scheme != "http") {
		return fmt.Errorf("registry token realm %q is not an http(s) URL", p["realm"])
	}
	if realm.Scheme == "http" && strings.HasPrefix(r.base, "https://") {
		return fmt.Errorf("registry token realm %q downgrades to plain HTTP", p["realm"])
	}
	q := realm.Query()
	if s := p["service"]; s != "" {
		q.Set("service", s)
	}
	scope := p["scope"]
	if scope == "" {
		scope = "repository:" + r.ref.Path + ":pull"
	}
	q.Set("scope", scope)
	realm.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, realm.String(), nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", r.ua)
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("registry token: %s", resp.Status)
	}
	b, err := readAllLimited(resp.Body, 1<<20)
	if err != nil {
		return err
	}
	var tr struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(b, &tr); err != nil {
		return fmt.Errorf("registry token: %w", err)
	}
	tok := tr.Token
	if tok == "" {
		tok = tr.AccessToken
	}
	if tok == "" {
		return errors.New("registry token response carries no token")
	}
	r.mu.Lock()
	r.token = tok
	r.mu.Unlock()
	return nil
}

func (r *RegistrySource) url(kind, object string) string {
	return r.base + "/v2/" + r.ref.Path + "/" + kind + "/" + object
}

// Resolve implements Source: it fetches the manifest or index the
// reference names, verifies it, and returns it inline in Data.
func (r *RegistrySource) Resolve(ctx context.Context) (Descriptor, error) {
	resp, err := r.do(ctx, r.url("manifests", r.ref.Object()), manifestAccept)
	if err != nil {
		return Descriptor{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Descriptor{}, registryError(resp)
	}
	b, err := readAllLimited(resp.Body, r.lim.MaxManifestBytes)
	if err != nil {
		return Descriptor{}, err
	}
	d := FromBytes(b)
	if r.ref.Digest != "" && d != r.ref.Digest {
		return Descriptor{}, fmt.Errorf("%w: registry served %s for %s", ErrDigest, d, r.ref.Digest)
	}
	if h := resp.Header.Get("Docker-Content-Digest"); h != "" && h != string(d) {
		return Descriptor{}, fmt.Errorf("%w: Docker-Content-Digest %s, content is %s", ErrDigest, h, d)
	}
	mt := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mt, ';'); i >= 0 {
		mt = strings.TrimSpace(mt[:i])
	}
	if !IsManifestType(mt) && !IsIndexType(mt) {
		switch Sniff("", b) {
		case KindIndex:
			mt = MediaTypeOCIIndex
		case KindManifest:
			mt = MediaTypeOCIManifest
		default:
			return Descriptor{}, invalid("registry served %q for a manifest", mt)
		}
	}
	return Descriptor{MediaType: mt, Digest: d, Size: int64(len(b)), Data: b}, nil
}

// Fetch implements Source.
func (r *RegistrySource) Fetch(ctx context.Context, d Descriptor) (io.ReadCloser, error) {
	if _, err := ParseDigest(string(d.Digest)); err != nil {
		return nil, err
	}
	kind, accept := "blobs", ""
	if IsManifestType(d.MediaType) || IsIndexType(d.MediaType) {
		kind, accept = "manifests", manifestAccept
	}
	resp, err := r.do(ctx, r.url(kind, string(d.Digest)), accept)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, registryError(resp)
	}
	if resp.ContentLength > d.Size {
		resp.Body.Close()
		return nil, fmt.Errorf("%w: registry announces %d bytes for %s, descriptor says %d", ErrTooLarge, resp.ContentLength, d.Digest, d.Size)
	}
	return resp.Body, nil
}
