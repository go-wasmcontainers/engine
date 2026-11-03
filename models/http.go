package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
)

// HTTPSource is a Source over a URL that honours Range requests, read in
// aligned chunks of ChunkSize (one chunk is kept), as the browser engine
// reads a model from the pinned Hugging Face URL. It follows redirects only
// to Hosts, never reads past the entry's size, and refuses a server that
// answers a range with the wrong length or a whole 200 body.
type HTTPSource struct {
	URL    string
	Client *http.Client
	Hosts  []string // nil: Hosts
	// ChunkSize is the range read size; zero means 16 MiB (HashChunk).
	ChunkSize int64

	size int64
	mu   sync.Mutex
	off  int64
	buf  []byte
}

// NewHTTPSource returns a source of the given size (from the registry; the
// server is never trusted for it). The URL's host must be allowed.
func NewHTTPSource(rawURL string, size int64, hosts []string, client *http.Client) (*HTTPSource, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	s := &HTTPSource{URL: rawURL, Client: client, Hosts: hosts, size: size, off: -1}
	if size <= 0 || size > MaxSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrSize, size)
	}
	if !s.hostOK(u.Hostname()) {
		return nil, fmt.Errorf("%w: %s", ErrHost, rawURL)
	}
	return s, nil
}

func (s *HTTPSource) hostOK(h string) bool { return Fetcher{Hosts: s.Hosts}.hostOK(h) }

// Size is the registry's size.
func (s *HTTPSource) Size() int64 { return s.size }

// Close drops the cached chunk.
func (s *HTTPSource) Close() error {
	s.mu.Lock()
	s.buf, s.off = nil, -1
	s.mu.Unlock()
	return nil
}

func (s *HTTPSource) chunk() int64 {
	if s.ChunkSize > 0 {
		return s.ChunkSize
	}
	return HashChunk
}

// ReadAt implements io.ReaderAt.
func (s *HTTPSource) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("models: negative offset")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for len(p) > 0 {
		if off >= s.size {
			return n, io.EOF
		}
		base := off / s.chunk() * s.chunk()
		if s.off != base {
			if err := s.load(base); err != nil {
				return n, err
			}
		}
		m := copy(p, s.buf[off-base:])
		p, off, n = p[m:], off+int64(m), n+m
	}
	return n, nil
}

func (s *HTTPSource) load(base int64) error {
	end := min(base+s.chunk(), s.size)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, s.URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(base, 10)+"-"+strconv.FormatInt(end-1, 10))
	c := http.Client{}
	if s.Client != nil {
		c = *s.Client
	}
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) > 5 {
			return errors.New("models: too many redirects")
		}
		if !s.hostOK(r.URL.Hostname()) {
			return fmt.Errorf("%w: redirect to %s", ErrHost, r.URL.Host)
		}
		return nil
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("models: range %d-%d of %s: %s (a range answer is required)", base, end-1, s.URL, resp.Status)
	}
	buf := make([]byte, end-base)
	if _, err := io.ReadFull(resp.Body, buf); err != nil {
		return fmt.Errorf("models: range %d-%d: %w", base, end-1, err)
	}
	var extra [1]byte
	if k, _ := resp.Body.Read(extra[:]); k != 0 {
		return fmt.Errorf("%w: the server sent more than the %d bytes asked for", ErrSize, end-base)
	}
	s.buf, s.off = buf, base
	return nil
}
