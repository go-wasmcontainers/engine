package daemon

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-wasmcontainers/engine/container"
	"github.com/go-wasmcontainers/engine/events"
	"github.com/go-wasmcontainers/engine/image"
)

// CommitRequest is POST /commit: a container's changes as a new image.
type CommitRequest struct {
	Container string
	// Repo and Tag name the image; with no Repo it stays addressable by ID
	// only (a <none> image).
	Repo, Tag string
	Comment   string
	Author    string
	// Config replaces the image config's "config" object (Docker's
	// semantics: the whole object); nil keeps the parent's, with the
	// container's command, environment, working directory and labels.
	Config *image.ExecConfig
	// Changes are Dockerfile instructions applied on top (commit -c).
	Changes []string
}

// CommitResult is what a commit made.
type CommitResult struct {
	ID   image.Digest // the config digest
	Name string       // empty for a <none> image
}

func digestOf(b []byte) image.Digest {
	h := sha256.Sum256(b)
	return image.Digest("sha256:" + hex.EncodeToString(h[:]))
}

// tagName normalises repo[:tag] for a tag operation: a digest reference
// cannot be a tag.
func tagName(repo, tag string) (string, error) {
	ref := repo
	if tag != "" {
		ref += ":" + tag
	}
	r, err := image.ParseReference(ref)
	if err != nil {
		return "", fmt.Errorf("%w: invalid reference %q: %v", container.ErrInvalid, ref, err)
	}
	if r.Digest != "" {
		return "", fmt.Errorf("%w: %q: an image can be tagged, not given a digest", container.ErrInvalid, ref)
	}
	return r.String(), nil
}

// Commit writes a container's changes as a new image: the parent's
// layers, plus one layer with the container's changes (OCI whiteouts) when
// there are any, and a config whose history records the commit (an empty
// one marked empty_layer). The image is proved to unpack before it is
// named.
func (d *Daemon) Commit(req CommitRequest) (CommitResult, error) {
	c, err := d.mgr.Get(req.Container)
	if err != nil {
		return CommitResult{}, err
	}
	name := ""
	if req.Repo != "" {
		if name, err = tagName(req.Repo, req.Tag); err != nil {
			return CommitResult{}, err
		}
	} else if req.Tag != "" {
		return CommitResult{}, fmt.Errorf("%w: a tag needs a repository", container.ErrInvalid)
	}
	sn := c.Snapshot()
	_, parent, err := d.ResolveImage(sn.Config.ImageID)
	if err != nil {
		return CommitResult{}, fmt.Errorf("the container's image: %w", err)
	}

	// The changes, as an OCI layer (uncompressed: its digest is its diff_id).
	var layer bytes.Buffer
	empty := len(c.FS().Diff()) == 0
	if !empty {
		if _, err := c.FS().WriteLayer(&layer); err != nil {
			return CommitResult{}, fmt.Errorf("%w: %v", container.ErrInvalid, err)
		}
		if int64(layer.Len()) > d.opt.Limits.MaxBlobBytes {
			return CommitResult{}, fmt.Errorf("%w: the changes are %d bytes, the layer limit %d", image.ErrTooLarge, layer.Len(), d.opt.Limits.MaxBlobBytes)
		}
	}

	// The config: the parent's, with the new "config", rootfs and history.
	cfg := *parent.Config
	cfg.LayerDigests = nil
	cfg.Architecture, cfg.OS = image.WasmPlatform.Architecture, image.WasmPlatform.OS
	cfg.RootFS = image.RootFS{Type: "layers", DiffIDs: append([]string{}, parent.DiffIDs()...)}
	cfg.History = append([]image.History{}, parent.Config.History...)
	if req.Config != nil {
		cfg.Config = *req.Config
	} else {
		ec := ExecConfig(parent)
		cfg.Config = ec
		cfg.Config.Entrypoint, cfg.Config.Cmd = sn.Config.Entrypoint, sn.Config.Cmd
		cfg.Config.Env, cfg.Config.WorkingDir = sn.Config.Env, sn.Config.WorkingDir
		if len(sn.Config.Labels) > 0 {
			cfg.Config.Labels = sn.Config.Labels
		}
	}
	if cfg.Config, err = ApplyChanges(cfg.Config, req.Changes); err != nil {
		return CommitResult{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	cfg.Created, cfg.Author = now, req.Author
	h := image.History{Created: now, CreatedBy: strings.Join(sn.Command(), " "), Author: req.Author, Comment: req.Comment, EmptyLayer: empty}
	cfg.History = append(cfg.History, h)
	layers := append([]image.Descriptor{}, parent.Manifest.Layers...)
	if !empty {
		ld := digestOf(layer.Bytes())
		cfg.RootFS.DiffIDs = append(cfg.RootFS.DiffIDs, string(ld))
		layers = append(layers, image.Descriptor{MediaType: image.MediaTypeOCILayer, Digest: ld, Size: int64(layer.Len())})
		if err := d.store.Ingest(layers[len(layers)-1], bytes.NewReader(layer.Bytes())); err != nil {
			return CommitResult{}, err
		}
	}
	cb, err := json.Marshal(cfg)
	if err != nil {
		return CommitResult{}, err
	}
	cdesc := image.Descriptor{MediaType: image.MediaTypeOCIConfig, Digest: digestOf(cb), Size: int64(len(cb))}
	mb, err := json.Marshal(image.Manifest{SchemaVersion: 2, MediaType: image.MediaTypeOCIManifest, Config: cdesc, Layers: layers})
	if err != nil {
		return CommitResult{}, err
	}
	mdesc := image.Descriptor{MediaType: image.MediaTypeOCIManifest, Digest: digestOf(mb), Size: int64(len(mb))}
	for _, b := range []struct {
		d image.Descriptor
		b []byte
	}{{cdesc, cb}, {mdesc, mb}} {
		if err := d.store.Ingest(b.d, bytes.NewReader(b.b)); err != nil {
			return CommitResult{}, err
		}
	}
	// Read back as any image is (limits, parsing), and prove it unpacks.
	img, err := d.store.Image(mdesc.Digest, d.opt.Limits)
	if err != nil {
		return CommitResult{}, err
	}
	if _, err := d.lower(img); err != nil {
		return CommitResult{}, err
	}
	if name != "" {
		err = d.store.Tag(name, image.Ref{Manifest: mdesc.Digest})
	} else {
		err = d.store.KeepDangling(mdesc.Digest)
	}
	if err != nil {
		return CommitResult{}, err
	}
	attrs := map[string]string{"comment": req.Comment, "imageID": string(img.ID())}
	if name != "" {
		attrs["imageName"] = name
	}
	d.events.Publish(events.Event{Type: events.Container, Action: "commit", ID: sn.ID, Attributes: attrs})
	return CommitResult{ID: img.ID(), Name: name}, nil
}

// TagImage gives the image source resolves to the name repo[:tag].
func (d *Daemon) TagImage(source, repo, tag string) (string, error) {
	_, img, err := d.ResolveImage(source)
	if err != nil {
		return "", err
	}
	name, err := tagName(repo, tag)
	if err != nil {
		return "", err
	}
	if err := d.store.Tag(name, image.Ref{Manifest: img.ManifestDigest}); err != nil {
		return "", err
	}
	d.events.Publish(events.Event{Type: events.Image, Action: "tag", ID: string(img.ID()), Attributes: map[string]string{"name": name}})
	return name, nil
}
