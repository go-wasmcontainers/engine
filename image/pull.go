package image

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// Source is somewhere images come from: a registry repository or an OCI
// image layout. Nothing a Source returns is trusted; Pull verifies it.
type Source interface {
	// Resolve returns the descriptor of the reference the source was opened
	// for: an index or a manifest. It may carry the content in Data.
	Resolve(ctx context.Context) (Descriptor, error)
	// Fetch returns the content of d.
	Fetch(ctx context.Context, d Descriptor) (io.ReadCloser, error)
}

// Progress receives human-readable status lines during a pull, keyed by a
// short id (a digest prefix, or "" for the image as a whole).
type Progress func(id, status string)

// PullResult describes a completed pull.
type PullResult struct {
	Image *Image
	// RepoDigest is the digest the reference resolved to.
	RepoDigest Digest
	// Downloaded reports whether any blob was new to the store.
	Downloaded bool
}

// Pull copies the wasip1/wasm image src resolves to into st, verifying
// every byte. It does not tag the image; the caller names it.
func Pull(ctx context.Context, st *Store, src Source, lim Limits, progress Progress) (*PullResult, error) {
	if progress == nil {
		progress = func(string, string) {}
	}
	top, err := src.Resolve(ctx)
	if err != nil {
		return nil, err
	}
	if err := ValidateDescriptor(&top); err != nil {
		return nil, err
	}
	res := &PullResult{RepoDigest: top.Digest}
	b, fresh, err := fetchDocument(ctx, st, src, top, lim.MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	res.Downloaded = res.Downloaded || fresh
	manDesc := top
	switch Sniff(top.MediaType, b) {
	case KindIndex:
		x, err := ParseIndex(b, lim)
		if err != nil {
			return nil, err
		}
		manDesc, err = SelectManifest(x, WasmPlatform)
		if err != nil {
			return nil, err
		}
		b, fresh, err = fetchDocument(ctx, st, src, manDesc, lim.MaxManifestBytes)
		if err != nil {
			return nil, err
		}
		res.Downloaded = res.Downloaded || fresh
	case KindManifest:
	default:
		return nil, invalid("%s is neither a manifest nor an index (media type %q)", top.Digest, top.MediaType)
	}
	if Sniff(manDesc.MediaType, b) != KindManifest {
		return nil, invalid("%s is not an image manifest", manDesc.Digest)
	}
	m, err := ParseManifest(b, lim)
	if err != nil {
		return nil, err
	}
	if !supportedConfig(m.Config.MediaType) {
		return nil, fmt.Errorf("unsupported config media type %q", m.Config.MediaType)
	}
	total := m.Config.Size
	for _, l := range m.Layers {
		if !SupportedLayer(l.MediaType) {
			return nil, fmt.Errorf("unsupported layer media type %q", l.MediaType)
		}
		if l.Size > lim.MaxBlobBytes {
			return nil, fmt.Errorf("%w: layer %s is %d bytes, limit %d", ErrTooLarge, l.Digest, l.Size, lim.MaxBlobBytes)
		}
		total += l.Size
		if total > lim.MaxImageBytes {
			return nil, fmt.Errorf("%w: image is more than %d bytes", ErrTooLarge, lim.MaxImageBytes)
		}
	}
	cb, fresh, err := fetchDocument(ctx, st, src, m.Config, lim.MaxConfigBytes)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	res.Downloaded = res.Downloaded || fresh
	cfg, err := ParseConfig(cb, lim)
	if err != nil {
		return nil, err
	}
	if err := CheckPlatform(cfg); err != nil {
		return nil, err
	}
	for _, l := range m.Layers {
		id := l.Digest.Short()
		if st.Has(l.Digest) {
			progress(id, "Already exists")
			continue
		}
		progress(id, "Pulling fs layer")
		if err := fetchBlob(ctx, st, src, l); err != nil {
			return nil, fmt.Errorf("layer %s: %w", l.Digest, err)
		}
		res.Downloaded = true
		progress(id, "Pull complete")
	}
	img := &Image{ManifestDigest: manDesc.Digest, Manifest: m, ConfigDigest: m.Config.Digest, Config: cfg,
		Wasm: m.Config.MediaType == MediaTypeWasmConfig, Size: total}
	res.Image = img
	return res, nil
}

func supportedConfig(mt string) bool {
	return mt == MediaTypeOCIConfig || mt == MediaTypeDockerConfig || mt == MediaTypeWasmConfig
}

// SupportedLayer reports whether the engine can unpack a layer media type.
func SupportedLayer(mt string) bool {
	switch mt {
	case MediaTypeOCILayer, MediaTypeOCILayerGz, MediaTypeDockerLayerGz, MediaTypeWasmLayer,
		MediaTypeOCILayerZst, MediaTypePkgxLayerZst:
		return true
	}
	return false
}

// fetchDocument returns the verified content of a small JSON document and
// stores it, reporting whether it was new.
func fetchDocument(ctx context.Context, st *Store, src Source, d Descriptor, max int64) ([]byte, bool, error) {
	if d.Size > max {
		return nil, false, fmt.Errorf("%w: %s is %d bytes, limit %d", ErrTooLarge, d.Digest, d.Size, max)
	}
	if st.Has(d.Digest) {
		b, err := st.ReadBlob(d.Digest, max)
		if err == nil && d.Digest.Verify(b) == nil {
			return b, false, nil
		}
	}
	var b []byte
	if d.Data != nil {
		b = d.Data // ValidateDescriptor verified it
	} else {
		rc, err := src.Fetch(ctx, d)
		if err != nil {
			return nil, false, err
		}
		defer rc.Close()
		b, err = io.ReadAll(NewVerifyingReader(rc, d.Digest, d.Size))
		if err != nil {
			return nil, false, err
		}
	}
	if err := st.Ingest(d, bytes.NewReader(b)); err != nil {
		return nil, false, err
	}
	return b, true, nil
}

func fetchBlob(ctx context.Context, st *Store, src Source, d Descriptor) error {
	rc, err := src.Fetch(ctx, d)
	if err != nil {
		return err
	}
	defer rc.Close()
	return st.Ingest(d, rc)
}
