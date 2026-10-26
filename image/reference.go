package image

import (
	"fmt"
	"regexp"
	"strings"
)

// The grammars of the distribution specification.
var (
	nameRE = regexp.MustCompile(`^[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*(/[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*)*$`)
	tagRE  = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9._-]{0,127}$`)
	hostRE = regexp.MustCompile(`^([a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)*|\[[0-9a-fA-F:]+\])(:[0-9]{1,5})?$`)
)

// Docker Hub normalisation, as github.com/distribution/reference does it
// (normalize.go, splitDockerDomain).
const (
	DefaultDomain       = "docker.io"
	legacyDefaultDomain = "index.docker.io"
	officialRepoPrefix  = "library/"
	DefaultTag          = "latest"
	// DockerHubRegistry is the host that actually serves docker.io.
	DockerHubRegistry = "registry-1.docker.io"
)

// Reference is a parsed image reference: domain/path[:tag][@digest].
type Reference struct {
	Domain string
	Path   string
	Tag    string
	Digest Digest
}

// ParseReference parses and normalises s ("hello" becomes
// docker.io/library/hello:latest). A reference with neither tag nor digest
// gets the tag "latest".
func ParseReference(s string) (Reference, error) {
	if s == "" || len(s) > 512 {
		return Reference{}, fmt.Errorf("invalid reference %q", s)
	}
	var r Reference
	rest := s
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		d, err := ParseDigest(rest[i+1:])
		if err != nil {
			return Reference{}, fmt.Errorf("invalid reference %q: %w", s, err)
		}
		r.Digest = d
		rest = rest[:i]
	}
	// A tag is after the last colon that follows the last slash.
	if i := strings.LastIndexByte(rest, ':'); i > strings.LastIndexByte(rest, '/') {
		r.Tag = rest[i+1:]
		rest = rest[:i]
		if !tagRE.MatchString(r.Tag) {
			return Reference{}, fmt.Errorf("invalid tag in reference %q", s)
		}
	}
	domain, path, ok := strings.Cut(rest, "/")
	switch {
	case !ok:
		r.Domain, r.Path = DefaultDomain, officialRepoPrefix+rest
	case domain == "localhost", strings.ContainsAny(domain, ".:"), strings.ToLower(domain) != domain:
		r.Domain, r.Path = domain, path
	default:
		r.Domain, r.Path = DefaultDomain, rest
	}
	if r.Domain == legacyDefaultDomain {
		r.Domain = DefaultDomain
	}
	if r.Domain == DefaultDomain && !strings.Contains(r.Path, "/") {
		r.Path = officialRepoPrefix + r.Path
	}
	if !hostRE.MatchString(r.Domain) {
		return Reference{}, fmt.Errorf("invalid registry host in reference %q", s)
	}
	if !nameRE.MatchString(r.Path) || len(r.Path) > 255 {
		return Reference{}, fmt.Errorf("invalid repository name in reference %q", s)
	}
	if r.Tag == "" && r.Digest == "" {
		r.Tag = DefaultTag
	}
	return r, nil
}

// Name is the familiar repository name: docker.io/library/x is "x",
// docker.io/u/x is "u/x", anything else keeps its domain.
func (r Reference) Name() string {
	if r.Domain == DefaultDomain {
		return strings.TrimPrefix(r.Path, officialRepoPrefix)
	}
	return r.Domain + "/" + r.Path
}

// String is the familiar form, "name:tag", "name@digest" or both.
func (r Reference) String() string {
	s := r.Name()
	if r.Tag != "" {
		s += ":" + r.Tag
	}
	if r.Digest != "" {
		s += "@" + string(r.Digest)
	}
	return s
}

// Object is what the registry is asked for: the digest if any, else the tag.
func (r Reference) Object() string {
	if r.Digest != "" {
		return string(r.Digest)
	}
	return r.Tag
}

// RegistryHost is the host to contact for r.
func (r Reference) RegistryHost() string {
	if r.Domain == DefaultDomain {
		return DockerHubRegistry
	}
	return r.Domain
}

// NormalizeName returns the familiar "name:tag" (or "name@digest") a store
// keys images by.
func NormalizeName(s string) (string, error) {
	r, err := ParseReference(s)
	if err != nil {
		return "", err
	}
	return r.String(), nil
}
