package image

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// Limits bounds what a pull may make the engine read and keep.
type Limits struct {
	// MaxManifestBytes bounds a manifest or an index document.
	MaxManifestBytes int64
	// MaxConfigBytes bounds an image config.
	MaxConfigBytes int64
	// MaxBlobBytes bounds one layer blob as stored (compressed).
	MaxBlobBytes int64
	// MaxLayers bounds the number of layers of one manifest.
	MaxLayers int
	// MaxIndexEntries bounds the number of manifests of an index.
	MaxIndexEntries int
	// MaxImageBytes bounds the sum of an image's blob sizes.
	MaxImageBytes int64
}

// DefaultLimits are generous for wasm images and small for anything else.
var DefaultLimits = Limits{
	MaxManifestBytes: 4 << 20,
	MaxConfigBytes:   4 << 20,
	MaxBlobBytes:     512 << 20,
	MaxLayers:        128,
	MaxIndexEntries:  1024,
	MaxImageBytes:    1 << 30,
}

// ErrInvalid is wrapped by every validation failure of a document.
var ErrInvalid = errors.New("invalid OCI document")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// IsManifestType reports whether mt names an image manifest.
func IsManifestType(mt string) bool {
	return mt == MediaTypeOCIManifest || mt == MediaTypeDockerManifest
}

// IsIndexType reports whether mt names an image index.
func IsIndexType(mt string) bool {
	return mt == MediaTypeOCIIndex || mt == MediaTypeDockerList
}

// validMediaType checks the RFC 6838 shape type/subtype with restricted-name
// characters, plus the parameters-free form the specs use.
func validMediaType(mt string) bool {
	if len(mt) == 0 || len(mt) > 255 {
		return false
	}
	typ, sub, ok := strings.Cut(mt, "/")
	if !ok || typ == "" || sub == "" || len(typ) > 127 || len(sub) > 127 {
		return false
	}
	ok1 := func(s string) bool {
		for i := 0; i < len(s); i++ {
			c := s[i]
			switch {
			case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			case strings.IndexByte("!#$&-^_.+", c) >= 0:
			default:
				return false
			}
		}
		return true
	}
	return ok1(typ) && ok1(sub)
}

// ValidateDescriptor checks a descriptor's required fields.
func ValidateDescriptor(d *Descriptor) error {
	if !validMediaType(d.MediaType) {
		return invalid("descriptor media type %q", d.MediaType)
	}
	if _, err := ParseDigest(string(d.Digest)); err != nil {
		return invalid("descriptor: %v", err)
	}
	if d.Size < 0 {
		return invalid("descriptor %s has negative size %d", d.Digest, d.Size)
	}
	if d.Data != nil {
		if int64(len(d.Data)) != d.Size {
			return invalid("descriptor %s: embedded data is %d bytes, size says %d", d.Digest, len(d.Data), d.Size)
		}
		if err := d.Digest.Verify(d.Data); err != nil {
			return invalid("descriptor embedded data: %v", err)
		}
	}
	return nil
}

func decodeStrict(b []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(v); err != nil {
		return invalid("%v", err)
	}
	if dec.More() {
		return invalid("trailing data after the JSON document")
	}
	return nil
}

// Kind tells which document a blob of JSON is.
type Kind int

const (
	KindUnknown Kind = iota
	KindManifest
	KindIndex
)

// Sniff decides whether b is a manifest or an index, from the declared media
// type when there is one, otherwise from the document's own mediaType field,
// otherwise from its shape (manifests have "layers", indexes "manifests").
func Sniff(declared string, b []byte) Kind {
	if i := strings.IndexByte(declared, ';'); i >= 0 {
		declared = strings.TrimSpace(declared[:i])
	}
	switch {
	case IsManifestType(declared):
		return KindManifest
	case IsIndexType(declared):
		return KindIndex
	}
	var probe struct {
		MediaType string           `json:"mediaType"`
		Manifests *json.RawMessage `json:"manifests"`
		Layers    *json.RawMessage `json:"layers"`
	}
	if json.Unmarshal(b, &probe) != nil {
		return KindUnknown
	}
	switch {
	case IsManifestType(probe.MediaType):
		return KindManifest
	case IsIndexType(probe.MediaType):
		return KindIndex
	case probe.Manifests != nil && probe.Layers == nil:
		return KindIndex
	case probe.Layers != nil && probe.Manifests == nil:
		return KindManifest
	}
	return KindUnknown
}

// ParseManifest decodes and validates an image manifest within lim.
func ParseManifest(b []byte, lim Limits) (*Manifest, error) {
	if int64(len(b)) > lim.MaxManifestBytes {
		return nil, fmt.Errorf("%w: manifest is %d bytes, limit %d", ErrTooLarge, len(b), lim.MaxManifestBytes)
	}
	var m Manifest
	if err := decodeStrict(b, &m); err != nil {
		return nil, err
	}
	if m.SchemaVersion != 2 {
		return nil, invalid("manifest schemaVersion %d, want 2", m.SchemaVersion)
	}
	if m.MediaType != "" && !IsManifestType(m.MediaType) {
		return nil, invalid("manifest mediaType %q", m.MediaType)
	}
	if err := ValidateDescriptor(&m.Config); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if len(m.Layers) > lim.MaxLayers {
		return nil, fmt.Errorf("%w: %d layers, limit %d", ErrTooLarge, len(m.Layers), lim.MaxLayers)
	}
	for i := range m.Layers {
		if err := ValidateDescriptor(&m.Layers[i]); err != nil {
			return nil, fmt.Errorf("layer %d: %w", i, err)
		}
	}
	if m.Config.MediaType == MediaTypeWasmConfig && len(m.Layers) != 1 {
		// The CNCF layout: consumers should reject more than one layer.
		return nil, invalid("wasm artifact has %d layers, want exactly 1", len(m.Layers))
	}
	return &m, nil
}

// ParseIndex decodes and validates an image index within lim.
func ParseIndex(b []byte, lim Limits) (*Index, error) {
	if int64(len(b)) > lim.MaxManifestBytes {
		return nil, fmt.Errorf("%w: index is %d bytes, limit %d", ErrTooLarge, len(b), lim.MaxManifestBytes)
	}
	var x Index
	if err := decodeStrict(b, &x); err != nil {
		return nil, err
	}
	if x.SchemaVersion != 2 {
		return nil, invalid("index schemaVersion %d, want 2", x.SchemaVersion)
	}
	if x.MediaType != "" && !IsIndexType(x.MediaType) {
		return nil, invalid("index mediaType %q", x.MediaType)
	}
	if x.Manifests == nil {
		return nil, invalid("index has no manifests property")
	}
	if len(x.Manifests) > lim.MaxIndexEntries {
		return nil, fmt.Errorf("%w: %d index entries, limit %d", ErrTooLarge, len(x.Manifests), lim.MaxIndexEntries)
	}
	for i := range x.Manifests {
		if err := ValidateDescriptor(&x.Manifests[i]); err != nil {
			return nil, fmt.Errorf("index entry %d: %w", i, err)
		}
	}
	return &x, nil
}

// ParseConfig decodes and validates an image or wasm config within lim.
func ParseConfig(b []byte, lim Limits) (*Config, error) {
	if int64(len(b)) > lim.MaxConfigBytes {
		return nil, fmt.Errorf("%w: config is %d bytes, limit %d", ErrTooLarge, len(b), lim.MaxConfigBytes)
	}
	var c Config
	if err := decodeStrict(b, &c); err != nil {
		return nil, err
	}
	if c.Architecture == "" || c.OS == "" {
		return nil, invalid("config lacks architecture or os")
	}
	return &c, nil
}
