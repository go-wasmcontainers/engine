package image

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// DirBackend keeps blobs in a directory:
//
//	blobs/sha256/<hex>   verified content, written once by rename
//	refs.json            the name table
type DirBackend struct{ root string }

// NewDirBackend creates (if needed) and opens a directory backend.
func NewDirBackend(root string) (*DirBackend, error) {
	for _, d := range []string{filepath.Join(root, "blobs", "sha256"), filepath.Join(root, "tmp")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	return &DirBackend{root: root}, nil
}

func validHex(hex string) bool {
	_, err := ParseDigest("sha256:" + hex)
	return err == nil
}

func (b *DirBackend) path(hex string) string {
	// hex is 64 hex digits (validHex): no separator, nothing to escape.
	return filepath.Join(b.root, "blobs", "sha256", hex)
}

// OpenBlob implements Backend.
func (b *DirBackend) OpenBlob(hex string) (io.ReadCloser, error) {
	if !validHex(hex) {
		return nil, ErrDigest
	}
	f, err := os.Open(b.path(hex))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// HasBlob implements Backend.
func (b *DirBackend) HasBlob(hex string) bool {
	if !validHex(hex) {
		return false
	}
	_, err := os.Stat(b.path(hex))
	return err == nil
}

// PutBlob implements Backend: the content reaches its final name only by
// rename, after the reader reached EOF without error.
func (b *DirBackend) PutBlob(hex string, r io.Reader) error {
	if !validHex(hex) {
		return ErrDigest
	}
	tmp, err := os.CreateTemp(filepath.Join(b.root, "tmp"), "ingest-")
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
	if _, err := io.Copy(tmp, r); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), b.path(hex)); err != nil {
		return err
	}
	ok = true
	return nil
}

// DeleteBlob implements Backend.
func (b *DirBackend) DeleteBlob(hex string) error {
	if !validHex(hex) {
		return ErrDigest
	}
	return os.Remove(b.path(hex))
}

// ListBlobs implements Backend.
func (b *DirBackend) ListBlobs() ([]string, error) {
	ents, err := os.ReadDir(filepath.Join(b.root, "blobs", "sha256"))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(ents))
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out, nil
}

// LoadRefs implements Backend.
func (b *DirBackend) LoadRefs() ([]byte, error) {
	raw, err := os.ReadFile(filepath.Join(b.root, "refs.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return raw, err
}

// SaveRefs implements Backend, by rename.
func (b *DirBackend) SaveRefs(raw []byte) error {
	tmp, err := os.CreateTemp(filepath.Join(b.root, "tmp"), "refs-")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(b.root, "refs.json"))
}

// MemoryBackend keeps everything in memory, bounded by MaxBytes.
type MemoryBackend struct {
	// MaxBytes bounds the total blob bytes (0 = unlimited).
	MaxBytes int64

	mu    sync.Mutex
	blobs map[string][]byte
	refs  []byte
	size  int64
}

// NewMemoryBackend returns an empty memory backend.
func NewMemoryBackend(maxBytes int64) *MemoryBackend {
	return &MemoryBackend{MaxBytes: maxBytes, blobs: map[string][]byte{}}
}

// OpenBlob implements Backend.
func (m *MemoryBackend) OpenBlob(hex string) (io.ReadCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	b, ok := m.blobs[hex]
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// HasBlob implements Backend.
func (m *MemoryBackend) HasBlob(hex string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.blobs[hex]
	return ok
}

// PutBlob implements Backend: the blob is published only after the whole
// (verifying) reader was consumed without error.
func (m *MemoryBackend) PutBlob(hex string, r io.Reader) error {
	if !validHex(hex) {
		return ErrDigest
	}
	var buf bytes.Buffer
	limit := int64(-1)
	m.mu.Lock()
	if m.MaxBytes > 0 {
		limit = m.MaxBytes - m.size
	}
	m.mu.Unlock()
	src := r
	if limit >= 0 {
		src = io.LimitReader(r, limit+1)
	}
	if _, err := io.Copy(&buf, src); err != nil {
		return err
	}
	if limit >= 0 && int64(buf.Len()) > limit {
		return fmt.Errorf("%w: the image store is full (%d bytes)", ErrTooLarge, m.MaxBytes)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.blobs[hex]; !ok {
		m.blobs[hex] = buf.Bytes()
		m.size += int64(buf.Len())
	}
	return nil
}

// DeleteBlob implements Backend.
func (m *MemoryBackend) DeleteBlob(hex string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if b, ok := m.blobs[hex]; ok {
		m.size -= int64(len(b))
		delete(m.blobs, hex)
	}
	return nil
}

// ListBlobs implements Backend.
func (m *MemoryBackend) ListBlobs() ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.blobs))
	for k := range m.blobs {
		out = append(out, k)
	}
	sort.Strings(out)
	return out, nil
}

// LoadRefs implements Backend.
func (m *MemoryBackend) LoadRefs() ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refs, nil
}

// SaveRefs implements Backend.
func (m *MemoryBackend) SaveRefs(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.refs = append([]byte(nil), b...)
	return nil
}
