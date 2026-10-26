package image

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// LayoutPrefix marks an OCI image layout reference: "oci:<dir>[:<ref.name>]".
const LayoutPrefix = "oci:"

// LayoutRef is a parsed layout reference.
type LayoutRef struct {
	Dir string
	// RefName selects the index.json entry whose
	// org.opencontainers.image.ref.name annotation equals it; empty means
	// "the only entry".
	RefName string
}

// IsLayoutRef reports whether s names an OCI image layout.
func IsLayoutRef(s string) bool { return strings.HasPrefix(s, LayoutPrefix) }

// ParseLayoutRef parses "oci:<dir>[:<ref.name>]". The ref name is what
// follows the last colon when it contains no slash, so a directory may hold
// colons as long as a ref name is given.
func ParseLayoutRef(s string) (LayoutRef, error) {
	rest, ok := strings.CutPrefix(s, LayoutPrefix)
	if !ok || rest == "" {
		return LayoutRef{}, fmt.Errorf("invalid layout reference %q", s)
	}
	var r LayoutRef
	if i := strings.LastIndexByte(rest, ':'); i >= 0 && !strings.ContainsAny(rest[i+1:], `/\`) {
		r.Dir, r.RefName = rest[:i], rest[i+1:]
	} else {
		r.Dir = rest
	}
	if r.Dir == "" {
		return LayoutRef{}, fmt.Errorf("invalid layout reference %q: empty directory", s)
	}
	return r, nil
}

// LocalName is the name a layout image is stored under: the ref name
// normalised as an image reference, or the directory's base name.
func (r LayoutRef) LocalName() (string, error) {
	n := r.RefName
	if n == "" {
		n = strings.ToLower(filepath.Base(filepath.Clean(r.Dir)))
	}
	return NormalizeName(n)
}

// LayoutSource reads an OCI image layout through an fs.FS.
type LayoutSource struct {
	fsys    fs.FS
	refName string
	lim     Limits
}

// OpenLayoutDir opens the layout directory a LayoutRef names.
func OpenLayoutDir(r LayoutRef, lim Limits) (*LayoutSource, error) {
	st, err := os.Stat(r.Dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", r.Dir)
	}
	return OpenLayout(os.DirFS(r.Dir), r.RefName, lim)
}

// OpenLayout opens a layout and checks its oci-layout marker.
func OpenLayout(fsys fs.FS, refName string, lim Limits) (*LayoutSource, error) {
	b, err := readFSLimited(fsys, "oci-layout", 64<<10)
	if err != nil {
		return nil, fmt.Errorf("not an OCI image layout: %w", err)
	}
	var marker map[string]json.RawMessage
	if err := json.Unmarshal(b, &marker); err != nil {
		return nil, fmt.Errorf("oci-layout: %w", err)
	}
	var v string
	if raw, ok := marker["imageLayoutVersion"]; !ok || json.Unmarshal(raw, &v) != nil || v == "" {
		return nil, errors.New("oci-layout: missing imageLayoutVersion")
	}
	return &LayoutSource{fsys: fsys, refName: refName, lim: lim}, nil
}

func readFSLimited(fsys fs.FS, name string, max int64) ([]byte, error) {
	f, err := fsys.Open(name)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAllLimited(f, max)
}

// Index reads and validates index.json.
func (l *LayoutSource) Index() (*Index, error) {
	b, err := readFSLimited(l.fsys, "index.json", l.lim.MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	return ParseIndex(b, l.lim)
}

// Resolve implements Source.
func (l *LayoutSource) Resolve(context.Context) (Descriptor, error) {
	x, err := l.Index()
	if err != nil {
		return Descriptor{}, err
	}
	if l.refName == "" {
		if len(x.Manifests) != 1 {
			return Descriptor{}, fmt.Errorf("layout holds %d entries; name one with oci:<dir>:<ref.name>", len(x.Manifests))
		}
		return x.Manifests[0], nil
	}
	var names []string
	for _, d := range x.Manifests {
		n := d.Annotations[AnnotationRefName]
		if n == l.refName {
			return d, nil
		}
		if n != "" {
			names = append(names, n)
		}
	}
	return Descriptor{}, fmt.Errorf("layout has no entry named %q (it has %s): %w", l.refName, strings.Join(names, ", "), ErrNotFound)
}

// Fetch implements Source.
func (l *LayoutSource) Fetch(_ context.Context, d Descriptor) (io.ReadCloser, error) {
	if _, err := ParseDigest(string(d.Digest)); err != nil {
		return nil, err
	}
	// A validated digest is "sha256:<hex>": the path cannot leave blobs/.
	f, err := l.fsys.Open(path.Join("blobs", d.Digest.Algorithm(), d.Digest.Encoded()))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("blob %s: %w", d.Digest, ErrNotFound)
	}
	return f, err
}
