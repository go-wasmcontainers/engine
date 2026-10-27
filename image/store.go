package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
)

// ErrNotFound is wrapped when a blob or a name is not in the store.
var ErrNotFound = errors.New("not found")

// Backend is where a Store keeps bytes: a directory on disk natively, a
// map in memory, or the browser's IndexedDB in the playground. Blobs are
// named by the encoded part of their sha256 digest; the Store has always
// verified a blob before PutBlob, and PutBlob must not make a blob visible
// unless its reader reached EOF without error (it is a verifying reader).
type Backend interface {
	OpenBlob(hex string) (io.ReadCloser, error)
	HasBlob(hex string) bool
	PutBlob(hex string, r io.Reader) error
	DeleteBlob(hex string) error
	ListBlobs() ([]string, error)
	// LoadRefs returns the saved name table, nil when there is none.
	LoadRefs() ([]byte, error)
	SaveRefs(b []byte) error
}

// Store is a content-addressed blob store with a table of names.
//
// A blob only ever becomes visible after its size and digest were checked,
// so anything found there can be trusted to be what its name says.
type Store struct {
	b Backend

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

// OpenStore opens (creating if needed) a store in a directory.
func OpenStore(root string) (*Store, error) {
	b, err := NewDirBackend(root)
	if err != nil {
		return nil, err
	}
	return NewStore(b)
}

// NewStore opens a store over a backend, loading its name table.
func NewStore(b Backend) (*Store, error) {
	s := &Store{b: b, refs: map[string]Ref{}}
	raw, err := b.LoadRefs()
	if err != nil {
		return nil, err
	}
	if raw != nil {
		if err := json.Unmarshal(raw, &s.refs); err != nil {
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

// Has reports whether the blob is present.
func (s *Store) Has(d Digest) bool {
	if _, err := ParseDigest(string(d)); err != nil {
		return false
	}
	return s.b.HasBlob(d.Encoded())
}

// Open opens a stored blob.
func (s *Store) Open(d Digest) (io.ReadCloser, error) {
	if _, err := ParseDigest(string(d)); err != nil {
		return nil, err
	}
	rc, err := s.b.OpenBlob(d.Encoded())
	if errors.Is(err, ErrNotFound) {
		return nil, fmt.Errorf("blob %s: %w", d, ErrNotFound)
	}
	return rc, err
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
	return s.b.PutBlob(desc.Digest.Encoded(), NewVerifyingReader(r, desc.Digest, desc.Size))
}

// danglingPrefix marks a name-table entry that keeps an image with no name
// (a commit without a repository, an image whose tag moved away): "@"
// followed by the manifest digest. No reference can have that form.
const danglingPrefix = "@"

// IsDangling reports a name-table entry that only keeps an unnamed image.
func IsDangling(name string) bool { return strings.HasPrefix(name, danglingPrefix) }

func (s *Store) referencedLocked(m Digest) bool {
	for n, r := range s.refs {
		if r.Manifest == m && !IsDangling(n) {
			return true
		}
	}
	return false
}

// Tag points name at a manifest. The manifest stops being dangling; the
// image name pointed at before becomes dangling if no other name has it,
// as Docker leaves a `<none>` image behind.
func (s *Store) Tag(name string, r Ref) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, had := s.refs[name]
	s.refs[name] = r
	delete(s.refs, danglingPrefix+string(r.Manifest))
	if had && old.Manifest != r.Manifest && !s.referencedLocked(old.Manifest) {
		s.refs[danglingPrefix+string(old.Manifest)] = Ref{Manifest: old.Manifest}
	}
	return s.saveLocked()
}

// KeepDangling keeps an unnamed image (by its manifest) from the GC, unless
// a name already has it.
func (s *Store) KeepDangling(m Digest) error {
	if _, err := ParseDigest(string(m)); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.referencedLocked(m) {
		return nil
	}
	s.refs[danglingPrefix+string(m)] = Ref{Manifest: m}
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
	return s.b.SaveRefs(b)
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
	all, err := s.b.ListBlobs()
	if err != nil {
		return nil, err
	}
	var gone []Digest
	for _, hex := range all {
		if keep[hex] {
			continue
		}
		d, err := ParseDigest("sha256:" + hex)
		if err != nil {
			continue // not ours; leave it
		}
		if err := s.b.DeleteBlob(hex); err != nil {
			return gone, err
		}
		gone = append(gone, d)
	}
	sort.Slice(gone, func(i, j int) bool { return gone[i] < gone[j] })
	return gone, nil
}
