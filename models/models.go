// Package models is the engine's read-only model mount (M7 of the LLM plan,
// go-wasmcontainers/gpu docs/llm-design.md): a container that asks for a model
// finds it at /models/NAME.gguf, read-only and range-readable, and can
// neither write to it nor name a URL.
//
// A model is a registry entry pinned by Hugging Face commit and sha256. The
// bytes come from a Source (natively a file of the host's cache, in the
// browser the OPFS cache filled from the pinned URL); before the first use
// the source's size is checked against the entry and against MaxSize, and its
// whole content against the entry's sha256. Only then is it mounted, as an
// external file of a frozen rootfs tree (rootfs.FS.AddExternal), so nothing
// is copied into memory.
package models

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/go-wasmcontainers/engine/rootfs"
)

// MaxSize bounds a model file: the 1.5 GiB GPU grant ceiling, plus the
// room a GGUF header and the unquantised tensors need.
const MaxSize int64 = 2 << 30

// HashChunk is the size of the reads that verify a source.
const HashChunk = 16 << 20

// Hosts are the only hosts a model may be fetched from: Hugging Face and the
// CDN its resolve URLs redirect to (the playground's CSP connect-src lists
// the same two).
var Hosts = []string{"huggingface.co", "us.aws.cdn.hf.co"}

// Errors.
var (
	ErrUnknown  = errors.New("models: unknown model")
	ErrSize     = errors.New("models: wrong or excessive size")
	ErrChecksum = errors.New("models: sha256 mismatch")
	ErrHost     = errors.New("models: host not allowed")
)

// Entry is one pinned model.
type Entry struct {
	Name     string // the mount name: /models/Name.gguf
	Repo     string // Hugging Face repository, "owner/name"
	Revision string // commit, never a branch
	File     string // file in the repository
	Size     int64
	SHA256   string // lowercase hex of the file (the LFS oid)
	License  string
}

// Registry is the list of pinned models. Entries are added by pull request,
// each with its commit and the sha256 read from the repository's LFS pointer.
var Registry = []Entry{
	{Name: "smollm2-135m-instruct-q4_0", Repo: "bartowski/SmolLM2-135M-Instruct-GGUF",
		Revision: "09816acd5d99df7be770d85ea30822623dab342c", File: "SmolLM2-135M-Instruct-Q4_0.gguf",
		Size: 91893088, SHA256: "bcc3af2849ad6095af57e9b5cd43775256efdc66e306acb529172f92d0c04b03", License: "apache-2.0"},
}

// Lookup finds an entry by name.
func Lookup(name string) (Entry, error) {
	for _, e := range Registry {
		if e.Name == name {
			return e, nil
		}
	}
	return Entry{}, fmt.Errorf("%w: %q", ErrUnknown, name)
}

// Check refuses a malformed entry: a name that is not a plain file name, a
// revision that is not a 40-digit commit, a size above MaxSize, a bad digest.
func (e Entry) Check() error {
	if e.Name == "" || strings.ContainsAny(e.Name, "/\\:,=\x00") || e.Name == "." || e.Name == ".." {
		return fmt.Errorf("models: bad name %q", e.Name)
	}
	if len(e.Revision) != 40 || !isHex(e.Revision) {
		return fmt.Errorf("models: %s: revision %q is not a 40-digit commit", e.Name, e.Revision)
	}
	if len(e.SHA256) != 64 || !isHex(e.SHA256) {
		return fmt.Errorf("models: %s: bad sha256", e.Name)
	}
	if e.Size <= 0 || e.Size > MaxSize {
		return fmt.Errorf("%w: %s: %d bytes", ErrSize, e.Name, e.Size)
	}
	if e.Repo == "" || e.File == "" || strings.Contains(e.File, "..") {
		return fmt.Errorf("models: %s: bad repository or file", e.Name)
	}
	return nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// URL is the pinned resolve URL (the commit, never main).
func (e Entry) URL() string {
	return "https://huggingface.co/" + e.Repo + "/resolve/" + e.Revision + "/" + url.PathEscape(e.File)
}

// FileName is the name under /models.
func (e Entry) FileName() string { return e.Name + ".gguf" }

// Source is a range-readable model file.
type Source interface {
	io.ReaderAt
	Size() int64
	Close() error
}

// FileSource is a Source over a host file.
type FileSource struct {
	f    *os.File
	size int64
}

// OpenFile opens a host file as a Source.
func OpenFile(path string) (*FileSource, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("models: %s is not a regular file", path)
	}
	return &FileSource{f, st.Size()}, nil
}

// ReadAt implements io.ReaderAt.
func (s *FileSource) ReadAt(p []byte, off int64) (int, error) { return s.f.ReadAt(p, off) }

// Size is the file's length.
func (s *FileSource) Size() int64 { return s.size }

// Close closes the file.
func (s *FileSource) Close() error { return s.f.Close() }

// Verify checks a source against an entry before it is used: the size first
// (so nothing is read from a source of the wrong or an excessive size), then
// the sha256 of the whole content, read in HashChunk pieces.
func Verify(src Source, e Entry) error {
	if err := e.Check(); err != nil {
		return err
	}
	if src.Size() != e.Size {
		return fmt.Errorf("%w: %s is %d bytes, the registry says %d", ErrSize, e.Name, src.Size(), e.Size)
	}
	h := sha256.New()
	buf := make([]byte, min(int64(HashChunk), e.Size))
	for off := int64(0); off < e.Size; {
		n := min(int64(len(buf)), e.Size-off)
		if _, err := src.ReadAt(buf[:n], off); err != nil {
			return fmt.Errorf("models: reading %s at %d: %w", e.Name, off, err)
		}
		h.Write(buf[:n])
		off += n
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
		return fmt.Errorf("%w: %s is %s, the registry says %s", ErrChecksum, e.Name, got, e.SHA256)
	}
	return nil
}

// Mount builds the tree mounted at /models: one external, read-only file per
// verified source. The tree is frozen. Every source must already be verified
// (use Open or Verify): Mount does not read it.
func Mount(srcs map[string]Source, entries []Entry) (*rootfs.FS, error) {
	fsys := rootfs.New()
	for _, e := range entries {
		s := srcs[e.Name]
		if s == nil {
			return nil, fmt.Errorf("models: no source for %s", e.Name)
		}
		if err := fsys.AddExternal("/"+e.FileName(), s, s.Size()); err != nil {
			return nil, err
		}
	}
	fsys.Freeze()
	return fsys, nil
}

// CacheDir is the native cache: os.UserCacheDir()/go-wasmcontainers/models.
func CacheDir() (string, error) {
	d, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "go-wasmcontainers", "models"), nil
}

// CachePath is where an entry lives in dir: named by its sha256, so a file
// can never be mistaken for another model.
func CachePath(dir string, e Entry) string { return filepath.Join(dir, e.SHA256+".gguf") }

type verified struct {
	size, mtime int64
}

var (
	vmu  sync.Mutex
	vmem = map[string]verified{}
)

// Open returns the verified source of an entry from the cache directory dir.
// A file already verified in this process (same path, size and modification
// time) is not hashed again.
func Open(dir string, e Entry) (Source, error) {
	if err := e.Check(); err != nil {
		return nil, err
	}
	p := CachePath(dir, e)
	s, err := OpenFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("models: %s is not in the cache %s: fetch it from %s (wctr model pull %s)", e.Name, dir, e.URL(), e.Name)
		}
		return nil, err
	}
	st, _ := s.f.Stat()
	key := verified{st.Size(), st.ModTime().UnixNano()}
	vmu.Lock()
	ok := vmem[p] == key
	vmu.Unlock()
	if !ok {
		if err := Verify(s, e); err != nil {
			s.Close()
			return nil, err
		}
		vmu.Lock()
		vmem[p] = key
		vmu.Unlock()
	}
	return s, nil
}

// Fetcher downloads entries into a cache directory.
type Fetcher struct {
	Client *http.Client // nil: http.DefaultClient with the host check
	Hosts  []string     // nil: Hosts
}

func (f Fetcher) hostOK(h string) bool {
	hs := f.Hosts
	if hs == nil {
		hs = Hosts
	}
	for _, a := range hs {
		if h == a {
			return true
		}
	}
	return false
}

// Fetch downloads e from its pinned URL into dir, refusing a host outside
// Hosts at the first request and at every redirect, a Content-Length above the
// entry's size, and a body that is longer than, or hashes differently from,
// the entry. The file appears in dir only once verified.
func (f Fetcher) Fetch(ctx context.Context, dir string, e Entry, rawURL string) error {
	if err := e.Check(); err != nil {
		return err
	}
	if rawURL == "" {
		rawURL = e.URL()
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Scheme != "https" && !(len(f.Hosts) > 0 && u.Scheme == "http") || !f.hostOK(u.Hostname()) {
		return fmt.Errorf("%w: %s", ErrHost, rawURL)
	}
	c := f.Client
	if c == nil {
		c = &http.Client{}
	}
	cc := *c
	cc.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return errors.New("models: too many redirects")
		}
		if !f.hostOK(req.URL.Hostname()) {
			return fmt.Errorf("%w: redirect to %s", ErrHost, req.URL.Host)
		}
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := cc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("models: %s: %s", rawURL, resp.Status)
	}
	if resp.ContentLength > e.Size {
		return fmt.Errorf("%w: the server announces %d bytes, the registry says %d", ErrSize, resp.ContentLength, e.Size)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "pull-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, e.Size+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if n != e.Size {
		return fmt.Errorf("%w: got %d bytes, the registry says %d", ErrSize, n, e.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != e.SHA256 {
		return fmt.Errorf("%w: %s is %s, the registry says %s", ErrChecksum, e.Name, got, e.SHA256)
	}
	return os.Rename(tmp.Name(), CachePath(dir, e))
}
