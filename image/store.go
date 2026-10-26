package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ErrNotFound is wrapped when a blob or a name is not in the store.
var ErrNotFound = errors.New("not found")

// Store is a content-addressed blob store on disk with a table of names.
//
// Layout under root:
//
//	blobs/sha256/<hex>   verified content, written once by rename
//	refs.json            name -> manifest digest
//
// A blob only ever reaches its final path after its size and digest were
// checked, so anything found there can be trusted to be what its name says.
type Store struct {
	root string

	mu   sync.Mutex
	refs map[string]Ref
}

// Ref is what a name points at.
type Ref struct {
	// Manifest is the platform image manifest.
	Manifest Digest `json:"manifest"`
	// RepoDigest is the digest the name resolved to at pull time (an index
	// or the manifest itself).
	RepoDigest Digest `json:"repoDigest,omitempty"`
}

// OpenStore opens (creating if needed) a store rooted at root.
func OpenStore(root string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(root, "blobs", "sha256"), 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(root, "tmp"), 0o700); err != nil {
		return nil, err
	}
	s := &Store{root: root, refs: map[string]Ref{}}
	b, err := os.ReadFile(filepath.Join(root, "refs.json"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	default:
		if err := json.Unmarshal(b, &s.refs); err != nil {
			return nil, fmt.Errorf("refs.json: %w", err)
		}
		for name, r := range s.refs {
			if _, err := ParseDigest(string(r.Manifest)); err != nil {
				return nil, fmt.Errorf("refs.json: %s: %w", name, err)
			}
		}
	}
	return s, nil
}

func (s *Store) blobPath(d Digest) string {
	// d is a validated Digest, so Encoded() is 64 hex digits: no separator,
	// no dot, nothing that can leave the directory.
	return filepath.Join(s.root, "blobs", "sha256", d.Encoded())
}

// Has reports whether the blob is present.
func (s *Store) Has(d Digest) bool {
	if _, err := ParseDigest(string(d)); err != nil {
		return false
	}
	_, err := os.Stat(s.blobPath(d))
	return err == nil
}

// Open opens a stored blob.
func (s *Store) Open(d Digest) (*os.File, error) {
	if _, err := ParseDigest(string(d)); err != nil {
		return nil, err
	}
	f, err := os.Open(s.blobPath(d))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("blob %s: %w", d, ErrNotFound)
	}
	return f, err
}

// ReadBlob reads a stored blob of at most max bytes.
func (s *Store) ReadBlob(d Digest, max int64) ([]byte, error) {
	f, err := s.Open(d)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readAllLimited(f, max)
}

// Ingest copies the content desc names from r into the store, verifying
// its size and digest. desc.Size must already have been checked against a
// limit by the caller.
func (s *Store) Ingest(desc Descriptor, r io.Reader) error {
	if _, err := ParseDigest(string(desc.Digest)); err != nil {
		return err
	}
	if s.Has(desc.Digest) {
		return nil
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "ingest-")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	if _, err := io.Copy(tmp, NewVerifyingReader(r, desc.Digest, desc.Size)); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), s.blobPath(desc.Digest)); err != nil {
		return err
	}
	ok = true
	return nil
}

// Tag points name at a manifest.
func (s *Store) Tag(name string, r Ref) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refs[name] = r
	return s.saveLocked()
}

// Untag removes a name; it reports whether it existed.
func (s *Store) Untag(name string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.refs[name]; !ok {
		return false, nil
	}
	delete(s.refs, name)
	return true, s.saveLocked()
}

// Resolve returns what name points at.
func (s *Store) Resolve(name string) (Ref, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.refs[name]
	return r, ok
}

// Names returns every name, sorted, with its ref.
func (s *Store) Names() []NamedRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]NamedRef, 0, len(s.refs))
	for n, r := range s.refs {
		out = append(out, NamedRef{Name: n, Ref: r})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// NamedRef is one row of the name table.
type NamedRef struct {
	Name string
	Ref
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.refs, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Join(s.root, "tmp"), "refs-")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(s.root, "refs.json"))
}

// Image loads the image a manifest digest names, re-validating what it reads.
func (s *Store) Image(manifest Digest, lim Limits) (*Image, error) {
	mb, err := s.ReadBlob(manifest, lim.MaxManifestBytes)
	if err != nil {
		return nil, err
	}
	m, err := ParseManifest(mb, lim)
	if err != nil {
		return nil, err
	}
	cb, err := s.ReadBlob(m.Config.Digest, lim.MaxConfigBytes)
	if err != nil {
		return nil, err
	}
	c, err := ParseConfig(cb, lim)
	if err != nil {
		return nil, err
	}
	img := &Image{ManifestDigest: manifest, Manifest: m, ConfigDigest: m.Config.Digest, Config: c,
		Wasm: m.Config.MediaType == MediaTypeWasmConfig}
	img.Size = m.Config.Size
	for _, l := range m.Layers {
		img.Size += l.Size
	}
	return img, nil
}

// GC deletes every blob no name reaches, and returns the digests deleted.
func (s *Store) GC(lim Limits) ([]Digest, error) {
	keep := map[string]bool{}
	for _, nr := range s.Names() {
		keep[nr.Manifest.Encoded()] = true
		if nr.RepoDigest != "" {
			keep[nr.RepoDigest.Encoded()] = true
		}
		img, err := s.Image(nr.Manifest, lim)
		if err != nil {
			// Keep everything when the graph cannot be read: deleting on a
			// partial view is how a GC loses data.
			return nil, fmt.Errorf("gc: %s: %w", nr.Name, err)
		}
		keep[img.ConfigDigest.Encoded()] = true
		for _, l := range img.Manifest.Layers {
			keep[l.Digest.Encoded()] = true
		}
	}
	dir := filepath.Join(s.root, "blobs", "sha256")
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var gone []Digest
	for _, e := range ents {
		if keep[e.Name()] {
			continue
		}
		d, err := ParseDigest("sha256:" + e.Name())
		if err != nil {
			continue // not ours; leave it
		}
		if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
			return gone, err
		}
		gone = append(gone, d)
	}
	return gone, nil
}
