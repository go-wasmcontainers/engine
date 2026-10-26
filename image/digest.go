// Package image is the OCI content side of the engine: content-addressed
// blobs, manifests, indexes and configs, pulled from an OCI image layout or
// an OCI distribution registry and kept in a local store.
//
// Everything that arrives from outside is treated as hostile: every read is
// bounded, every blob is verified against its digest before it is used, and
// every document is validated before it is trusted.
package image

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"strings"
)

// Digest is a validated content identifier, "sha256:<64 lower-case hex>".
//
// Only sha256 is accepted: descriptor.md makes it the one algorithm an
// implementation MUST support, and refusing the rest keeps the set of
// strings that can become a file name in the store closed.
type Digest string

// ErrDigest is wrapped by every digest syntax or verification failure.
var ErrDigest = errors.New("digest")

// ParseDigest validates s and returns it as a Digest.
func ParseDigest(s string) (Digest, error) {
	alg, enc, ok := strings.Cut(s, ":")
	if !ok {
		return "", fmt.Errorf("%w: %q has no algorithm", ErrDigest, s)
	}
	if alg != "sha256" {
		return "", fmt.Errorf("%w: unsupported algorithm %q", ErrDigest, alg)
	}
	if len(enc) != 64 {
		return "", fmt.Errorf("%w: sha256 encoded part must be 64 hex digits, got %d", ErrDigest, len(enc))
	}
	for i := 0; i < len(enc); i++ {
		c := enc[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", fmt.Errorf("%w: invalid character %q in %q", ErrDigest, c, s)
		}
	}
	return Digest(s), nil
}

// FromBytes returns the sha256 digest of b.
func FromBytes(b []byte) Digest {
	sum := sha256.Sum256(b)
	return Digest("sha256:" + hex.EncodeToString(sum[:]))
}

// Algorithm returns the part before the colon.
func (d Digest) Algorithm() string { a, _, _ := strings.Cut(string(d), ":"); return a }

// Encoded returns the hex part after the colon.
func (d Digest) Encoded() string { _, e, _ := strings.Cut(string(d), ":"); return e }

// String implements fmt.Stringer.
func (d Digest) String() string { return string(d) }

// Short returns the first 12 hex digits, the form `docker images` shows.
func (d Digest) Short() string {
	e := d.Encoded()
	if len(e) > 12 {
		return e[:12]
	}
	return e
}

// Verify checks that b is exactly the content d names.
func (d Digest) Verify(b []byte) error {
	if got := FromBytes(b); got != d {
		return fmt.Errorf("%w mismatch: want %s, got %s", ErrDigest, d, got)
	}
	return nil
}

// ErrTooLarge is wrapped when content exceeds its declared size or a limit.
var ErrTooLarge = errors.New("content too large")

// verifyingReader reads exactly size bytes and, at the end, checks the
// digest. A short stream, a long stream and a wrong digest are all errors;
// no byte past size is ever handed to the caller.
type verifyingReader struct {
	r      io.Reader
	want   Digest
	size   int64
	n      int64
	h      hash.Hash
	err    error
	closer io.Closer
}

// NewVerifyingReader wraps r so that reading it to EOF proves it is exactly
// size bytes with digest d. Reading fails with ErrTooLarge as soon as a byte
// past size is seen, and with ErrDigest at EOF when the content differs.
func NewVerifyingReader(r io.Reader, d Digest, size int64) io.Reader {
	return &verifyingReader{r: r, want: d, size: size, h: sha256.New()}
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.err != nil {
		return 0, v.err
	}
	// Ask for one byte more than remains so an over-long stream is seen.
	remain := v.size - v.n
	if int64(len(p)) > remain+1 {
		p = p[:remain+1]
	}
	n, err := v.r.Read(p)
	if v.n+int64(n) > v.size {
		v.err = fmt.Errorf("%w: %s is longer than its declared %d bytes", ErrTooLarge, v.want, v.size)
		return 0, v.err
	}
	v.n += int64(n)
	v.h.Write(p[:n])
	if err == io.EOF {
		if v.n != v.size {
			v.err = fmt.Errorf("%w: %s is %d bytes, declared %d", ErrDigest, v.want, v.n, v.size)
			return n, v.err
		}
		got := Digest("sha256:" + hex.EncodeToString(v.h.Sum(nil)))
		if got != v.want {
			v.err = fmt.Errorf("%w mismatch: want %s, got %s", ErrDigest, v.want, got)
			return n, v.err
		}
		v.err = io.EOF
	} else if err != nil {
		v.err = err
	}
	return n, err
}

// readAllLimited reads r to EOF, failing with ErrTooLarge past max bytes.
func readAllLimited(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrTooLarge, max)
	}
	return b, nil
}
