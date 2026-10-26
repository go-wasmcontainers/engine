// Package state is where the engine keeps what must survive a restart (a
// daemon restart natively, a page reload in a browser): containers, their
// logs and their writable layers. It is a small key-value Store with three
// implementations — a directory (wctrd's --data-root), memory (tests), and
// IndexedDB (the playground, outside this module) — and a sealed record
// format that detects a torn or corrupted value instead of trusting it.
//
// Keys are "<kind>/<name>", kind lower-case letters, name lower-case hex or
// letters, digits, '.', '_' and '-' (at most 128 bytes in all), so a key is
// also a safe relative file path.
package state

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Errors a Store returns.
var (
	ErrNotFound = errors.New("state: not found")
	// ErrNoSpace wraps a store's quota failure (QuotaExceededError in a
	// browser, ENOSPC natively). A Put that fails with it wrote nothing.
	ErrNoSpace = errors.New("state: no space left")
	// ErrCorrupt is a value whose seal does not match its content.
	ErrCorrupt = errors.New("state: corrupted record")
	ErrBadKey  = errors.New("state: invalid key")
)

// Store keeps values by key. Put is atomic per key: after a crash a key
// holds the old value or the new one, never a mix. Implementations are
// safe for concurrent use.
type Store interface {
	Get(key string) ([]byte, error)
	Put(key string, value []byte) error
	Delete(key string) error // nil when absent
	Keys(prefix string) ([]string, error)
}

// Describer is an optional Store method naming where it keeps values and
// how many bytes they hold (-1: unknown), for `system df`.
type Describer interface {
	Describe() (kind string, bytes int64)
}

var keyRE = regexp.MustCompile(`^[a-z]+/[a-z0-9][a-z0-9._-]*$`)

// CheckKey refuses keys that are not "<kind>/<name>" as documented.
func CheckKey(k string) error {
	if len(k) > 128 || !keyRE.MatchString(k) || strings.Contains(k, "..") {
		return fmt.Errorf("%w: %q", ErrBadKey, k)
	}
	return nil
}

// Mem is a Store in memory, bounded by MaxBytes (0 = unbounded).
type Mem struct {
	MaxBytes int64

	mu   sync.Mutex
	m    map[string][]byte
	size int64
}

func (s *Mem) Get(k string) ([]byte, error) {
	if err := CheckKey(k); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m[k]
	if !ok {
		return nil, ErrNotFound
	}
	return append([]byte(nil), v...), nil
}

func (s *Mem) Put(k string, v []byte) error {
	if err := CheckKey(k); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	grow := int64(len(v)) - int64(len(s.m[k]))
	if s.MaxBytes > 0 && s.size+grow > s.MaxBytes {
		return fmt.Errorf("%w: %d bytes over a %d-byte store", ErrNoSpace, s.size+grow, s.MaxBytes)
	}
	s.m[k] = append([]byte(nil), v...)
	s.size += grow
	return nil
}

func (s *Mem) Delete(k string) error {
	if err := CheckKey(k); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.size -= int64(len(s.m[k]))
	delete(s.m, k)
	return nil
}

func (s *Mem) Keys(prefix string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.m {
		if strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out, nil
}

// Describe implements Describer.
func (s *Mem) Describe() (string, int64) { return "memory (nothing survives a restart)", s.Size() }

// Size is the bytes held.
func (s *Mem) Size() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.size
}

// A sealed value is a header line, then the payload:
//
//	wctr-state 1 <kind> <sha256 of payload, hex> <payload length>\n
//
// The header names what the value is, so one kind of record can never be
// read as another, and the digest catches a torn write or a flipped byte.
const sealMagic = "wctr-state 1 "

var kindRE = regexp.MustCompile(`^[a-z][a-z-]{0,31}$`)

// Seal wraps payload as a value of the given kind.
func Seal(kind string, payload []byte) []byte {
	if !kindRE.MatchString(kind) {
		panic("state: invalid kind " + strconv.Quote(kind))
	}
	sum := sha256.Sum256(payload)
	var b bytes.Buffer
	b.Grow(len(sealMagic) + len(kind) + 64 + 24 + len(payload))
	fmt.Fprintf(&b, "%s%s %s %d\n", sealMagic, kind, hex.EncodeToString(sum[:]), len(payload))
	b.Write(payload)
	return b.Bytes()
}

// Unseal checks a sealed value of the given kind and returns its payload.
// Anything else — another kind, a bad digest, a payload over max bytes, a
// truncated value — is ErrCorrupt.
func Unseal(kind string, v []byte, max int) ([]byte, error) {
	nl := bytes.IndexByte(v, '\n')
	if nl < 0 || nl > 256 {
		return nil, fmt.Errorf("%w: no header", ErrCorrupt)
	}
	f := strings.Split(string(v[:nl]), " ")
	if len(f) != 5 || f[0]+" "+f[1]+" " != sealMagic {
		return nil, fmt.Errorf("%w: bad header", ErrCorrupt)
	}
	if f[2] != kind {
		return nil, fmt.Errorf("%w: a %q record, not %q", ErrCorrupt, f[2], kind)
	}
	n, err := strconv.Atoi(f[4])
	if err != nil || n < 0 || n > max || strconv.Itoa(n) != f[4] { // canonical only: no "+5", no "05"
		return nil, fmt.Errorf("%w: length %q (at most %d)", ErrCorrupt, f[4], max)
	}
	payload := v[nl+1:]
	if len(payload) != n {
		return nil, fmt.Errorf("%w: %d bytes, header says %d", ErrCorrupt, len(payload), n)
	}
	sum := sha256.Sum256(payload)
	if hex.EncodeToString(sum[:]) != f[3] {
		return nil, fmt.Errorf("%w: digest mismatch", ErrCorrupt)
	}
	return payload, nil
}

// BlobKey is the key of content-addressed data: "blob/<sha256 hex>".
func BlobKey(data []byte) string {
	sum := sha256.Sum256(data)
	return "blob/" + hex.EncodeToString(sum[:])
}

// GetBlob reads a blob and checks it against its key.
func GetBlob(s Store, key string, max int) ([]byte, error) {
	v, err := s.Get(key)
	if err != nil {
		return nil, err
	}
	if len(v) > max {
		return nil, fmt.Errorf("%w: blob of %d bytes (at most %d)", ErrCorrupt, len(v), max)
	}
	if BlobKey(v) != key {
		return nil, fmt.Errorf("%w: %s does not match its content", ErrCorrupt, key)
	}
	return v, nil
}
