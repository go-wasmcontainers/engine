package daemon

import (
	"context"
	"fmt"
	"io"

	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/events"
	"github.com/go-wasmcontainers/engine/image"
)

// Loaded is one image a load stored: its name (empty for an entry with no
// org.opencontainers.image.ref.name, kept as a <none> image) and ID.
type Loaded struct {
	Name string
	ID   image.Digest
}

// entrySource is a layout entry as a pull source.
type entrySource struct {
	*image.LayoutSource
	d image.Descriptor
}

func (e entrySource) Resolve(context.Context) (image.Descriptor, error) { return e.d, nil }

// Load stores every image of an OCI image layout sent as a tar (POST
// /images/load, what a builder exports): each index.json entry is pulled
// from the in-memory layout with every check a pull makes, proved to
// unpack, and named by its org.opencontainers.image.ref.name (a full image
// reference, or a tag of the layout's name). Nothing on the host is read.
func (d *Daemon) Load(ctx context.Context, r io.Reader, progress image.Progress) ([]Loaded, error) {
	fsys, err := image.ReadLayoutTar(r, d.opt.Limits)
	if err != nil {
		return nil, err
	}
	ls, err := image.OpenLayout(fsys, "", d.opt.Limits)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", container.ErrInvalid, err)
	}
	idx, err := ls.Index()
	if err != nil {
		return nil, fmt.Errorf("%w: index.json: %v", container.ErrInvalid, err)
	}
	if len(idx.Manifests) == 0 {
		return nil, fmt.Errorf("%w: the layout's index lists no image", container.ErrInvalid)
	}
	var out []Loaded
	for _, desc := range idx.Manifests {
		name := ""
		// containerd and Docker export the full name here; the OCI ref.name
		// is the fallback.
		rn := desc.Annotations["io.containerd.image.name"]
		if rn == "" {
			rn = desc.Annotations[image.AnnotationRefName]
		}
		if rn != "" {
			if name, err = image.NormalizeName(rn); err != nil {
				return out, fmt.Errorf("%w: ref.name %q: %v", container.ErrInvalid, rn, err)
			}
		}
		res, err := image.Pull(ctx, d.store, entrySource{ls, desc}, d.opt.Limits, progress)
		if err != nil {
			return out, err
		}
		if _, err := d.lower(res.Image); err != nil {
			return out, err
		}
		if name != "" {
			err = d.store.Tag(name, image.Ref{Manifest: res.Image.ManifestDigest})
		} else {
			err = d.store.KeepDangling(res.Image.ManifestDigest)
		}
		if err != nil {
			return out, err
		}
		id := res.Image.ID()
		d.events.Publish(events.Event{Type: events.Image, Action: "load", ID: string(id), Attributes: map[string]string{"name": name}})
		out = append(out, Loaded{Name: name, ID: id})
	}
	return out, nil
}
