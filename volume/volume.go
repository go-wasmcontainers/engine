// Package volume is named volumes: trees that outlive the containers that
// mount them (run -v NAME:/path[:ro]), kept in the state store like
// containers are. A volume is a writable rootfs.FS with a quota; it is
// mounted into a container as a WASI preopen (runtime.Mount), so two
// containers mounting one volume share one tree.
package volume

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/state"
)

// Errors the API maps to statuses.
var (
	ErrNotFound = errors.New("no such volume")
	ErrInUse    = errors.New("volume is in use")
	ErrInvalid  = errors.New("invalid volume")
	ErrTooMany  = errors.New("too many volumes")
)

// nameRE is Docker's volume name grammar.
var nameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]+$`)

// ValidName reports whether name may name a volume.
func ValidName(name string) bool { return len(name) <= 128 && nameRE.MatchString(name) }

// Options configure a Manager.
type Options struct {
	// Store keeps volumes across restarts (nil: memory only).
	Store state.Store
	// Quota bounds one volume (default 64 MiB, 16384 entries).
	Quota rootfs.Quota
	// Max bounds how many volumes exist (default 64).
	Max int
	// SaveEvery is how often changed volumes are stored (default 2s).
	SaveEvery time.Duration
}

// Volume is one named tree.
type Volume struct {
	Name      string
	CreatedAt time.Time
	Labels    map[string]string
	// Anonymous volumes were created without a name (prune removes them
	// by default).
	Anonymous bool
	fs        *rootfs.FS

	saved   uint64 // fs.Changes() at the last store, guarded by Manager.pmu
	blobKey string
}

// FS is the volume's tree.
func (v *Volume) FS() *rootfs.FS { return v.fs }

// Size is the bytes it holds.
func (v *Volume) Size() int64 { b, _ := v.fs.Usage(); return b }

// Manager owns every volume.
type Manager struct {
	opt Options

	mu   sync.Mutex
	vols map[string]*Volume
	err  error

	pmu  sync.Mutex
	stop chan struct{}
}

// New returns a manager; Restore loads stored volumes.
func New(opt Options) *Manager {
	if opt.Quota.MaxBytes == 0 {
		opt.Quota.MaxBytes = 64 << 20
	}
	if opt.Quota.MaxFiles == 0 {
		opt.Quota.MaxFiles = 1 << 14
	}
	if opt.Max <= 0 {
		opt.Max = 64
	}
	if opt.SaveEvery <= 0 {
		opt.SaveEvery = 2 * time.Second
	}
	m := &Manager{opt: opt, vols: map[string]*Volume{}, stop: make(chan struct{})}
	if opt.Store != nil {
		go m.saver()
	}
	return m
}

func newTree(q rootfs.Quota) *rootfs.FS {
	// A clone of an empty frozen tree: the quota applies, and WriteDiff
	// writes exactly what the volume holds.
	base := rootfs.New()
	base.Freeze()
	return base.Clone(q)
}

// Create makes a volume, or returns the existing one of that name
// (Docker's create is idempotent). An empty name makes an anonymous one.
func (m *Manager) Create(name string, labels map[string]string) (*Volume, bool, error) {
	anon := name == ""
	if anon {
		b := make([]byte, 32)
		rand.Read(b)
		name = hex.EncodeToString(b)
	}
	if !ValidName(name) {
		return nil, false, fmt.Errorf("%w: %q includes invalid characters for a local volume name, only [a-zA-Z0-9][a-zA-Z0-9_.-] are allowed", ErrInvalid, name)
	}
	m.mu.Lock()
	if v := m.vols[name]; v != nil {
		m.mu.Unlock()
		return v, false, nil
	}
	if len(m.vols) >= m.opt.Max {
		m.mu.Unlock()
		return nil, false, fmt.Errorf("%w: the limit is %d", ErrTooMany, m.opt.Max)
	}
	v := &Volume{Name: name, CreatedAt: time.Now().UTC(), Labels: labels, Anonymous: anon, fs: newTree(m.opt.Quota)}
	m.vols[name] = v
	m.mu.Unlock()
	m.pmu.Lock()
	m.setErr(m.saveLocked(v, true))
	m.pmu.Unlock()
	return v, true, nil
}

// Get finds a volume.
func (m *Manager) Get(name string) (*Volume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if v := m.vols[name]; v != nil {
		return v, nil
	}
	return nil, fmt.Errorf("%w: %s", ErrNotFound, name)
}

// List returns volumes by name.
func (m *Manager) List() []*Volume {
	m.mu.Lock()
	out := make([]*Volume, 0, len(m.vols))
	for _, v := range m.vols {
		out = append(out, v)
	}
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Remove deletes a volume that no container uses.
func (m *Manager) Remove(name string, inUse func(string) []string) error {
	v, err := m.Get(name)
	if err != nil {
		return err
	}
	if users := inUse(name); len(users) > 0 {
		return fmt.Errorf("%w: remove %s: volume is in use - [%s]", ErrInUse, name, strings.Join(users, ", "))
	}
	m.mu.Lock()
	delete(m.vols, name)
	m.mu.Unlock()
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if m.opt.Store != nil {
		m.opt.Store.Delete(recordKey(name))
		if v.blobKey != "" {
			m.opt.Store.Delete(v.blobKey)
		}
	}
	return nil
}

// Prune removes unused volumes: anonymous ones, or with all every one.
func (m *Manager) Prune(all bool, inUse func(string) []string) ([]string, int64) {
	var gone []string
	var bytes int64
	for _, v := range m.List() {
		if (!all && !v.Anonymous) || len(inUse(v.Name)) > 0 {
			continue
		}
		size := v.Size()
		if m.Remove(v.Name, inUse) == nil {
			gone = append(gone, v.Name)
			bytes += size
		}
	}
	return gone, bytes
}

// PersistError is the last failure to store a volume.
func (m *Manager) PersistError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.err
}

func (m *Manager) setErr(err error) {
	m.mu.Lock()
	m.err = err
	m.mu.Unlock()
}

const (
	recordKind = "volume"
	blobKind   = "vblob"
	maxRecord  = 64 << 10
)

// recordKey names a volume's record. Volume names may hold capitals and
// be 128 bytes long, which keys may not, so the key is the name's sha256
// (the record holds the name, and Restore checks they agree).
func recordKey(name string) string {
	sum := sha256.Sum256([]byte(name))
	return recordKind + "/" + hex.EncodeToString(sum[:])
}

type record struct {
	Name      string            `json:"name"`
	CreatedAt time.Time         `json:"createdAt"`
	Labels    map[string]string `json:"labels,omitempty"`
	Anonymous bool              `json:"anonymous,omitempty"`
	Blob      string            `json:"blob,omitempty"`
	Usage     rootfs.Usage      `json:"usage"`
}

var vblobRE = regexp.MustCompile(`^vblob/[0-9a-f]{64}$`)

// DecodeRecord reads a stored volume record (exported for the fuzzer).
func DecodeRecord(b []byte) (*record, error) {
	p, err := state.Unseal(recordKind, b, maxRecord)
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(p, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", state.ErrCorrupt, err)
	}
	if !ValidName(r.Name) || (r.Blob != "" && !vblobRE.MatchString(r.Blob)) {
		return nil, fmt.Errorf("%w: bad volume record", state.ErrCorrupt)
	}
	return &r, nil
}

// saveLocked stores v: its content (when changed or force), then its
// record; the old content blob goes once the record no longer names it.
func (m *Manager) saveLocked(v *Volume, force bool) error {
	s := m.opt.Store
	if s == nil {
		return nil
	}
	changes := v.fs.Changes()
	if !force && changes == v.saved {
		return nil
	}
	var buf bytes.Buffer
	u, err := v.fs.WriteDiff(&buf)
	if err != nil {
		return err
	}
	key := ""
	if buf.Len() > 1024 {
		key = state.ContentKey(blobKind, buf.Bytes())
		if err := s.Put(key, buf.Bytes()); err != nil {
			return err
		}
	}
	m.mu.Lock()
	r := record{Name: v.Name, CreatedAt: v.CreatedAt, Labels: v.Labels, Anonymous: v.Anonymous, Blob: key, Usage: u}
	live := m.vols[v.Name] == v
	m.mu.Unlock()
	if !live {
		return nil
	}
	raw, _ := json.Marshal(r)
	if err := s.Put(recordKey(v.Name), state.Seal(recordKind, raw)); err != nil {
		return err
	}
	if old := v.blobKey; old != "" && old != key {
		s.Delete(old)
	}
	v.blobKey, v.saved = key, changes
	return nil
}

// saver stores changed volumes every SaveEvery.
func (m *Manager) saver() {
	t := time.NewTicker(m.opt.SaveEvery)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			m.Flush(context.Background())
		case <-m.stop:
			return
		}
	}
}

// Flush stores every volume that changed since its last store.
func (m *Manager) Flush(ctx context.Context) error {
	if m.opt.Store == nil {
		return nil
	}
	m.pmu.Lock()
	defer m.pmu.Unlock()
	var first error
	for _, v := range m.List() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := m.saveLocked(v, false); err != nil && first == nil {
			first = err
		}
	}
	m.setErr(first)
	return first
}

// Close stores what changed and stops the saver.
func (m *Manager) Close(ctx context.Context) error {
	select {
	case <-m.stop:
	default:
		close(m.stop)
	}
	return m.Flush(ctx)
}

// Restore loads stored volumes; a record or content that fails its
// checks is dropped (the volume, or its content) and reported.
func (m *Manager) Restore(lim rootfs.Limits) ([]string, error) {
	s := m.opt.Store
	if s == nil {
		return nil, nil
	}
	m.pmu.Lock()
	defer m.pmu.Unlock()
	keys, err := s.Keys(recordKind + "/")
	if err != nil {
		return nil, err
	}
	var repaired []string
	named := map[string]bool{}
	for _, k := range keys {
		b, err := s.Get(k)
		if errors.Is(err, state.ErrNotFound) {
			continue
		}
		if err != nil {
			return repaired, err
		}
		r, err := DecodeRecord(b)
		if err == nil && recordKey(r.Name) != k {
			err = errors.New("record stored under another key")
		}
		if err != nil {
			repaired = append(repaired, fmt.Sprintf("%s: %v; dropped", k, err))
			s.Delete(k)
			continue
		}
		v := &Volume{Name: r.Name, CreatedAt: r.CreatedAt, Labels: r.Labels, Anonymous: r.Anonymous, fs: newTree(m.opt.Quota)}
		if r.Blob != "" {
			blob, err := state.GetBlob(s, r.Blob, int(min(lim.MaxBytes+lim.MaxBytes/4+1<<20, 1<<30)))
			if err == nil {
				err = v.fs.ApplyDiff(bytes.NewReader(blob), r.Usage, lim)
			}
			if err != nil {
				repaired = append(repaired, fmt.Sprintf("volume %s: content: %v; it is empty now", r.Name, err))
				v.fs = newTree(m.opt.Quota)
				r.Blob = ""
			}
		}
		v.blobKey, v.saved = r.Blob, v.fs.Changes()
		if r.Blob != "" {
			named[r.Blob] = true
		}
		m.mu.Lock()
		m.vols[v.Name] = v
		m.mu.Unlock()
	}
	// Content no record names: left by a crash between content and record.
	if blobs, err := s.Keys(blobKind + "/"); err == nil {
		for _, k := range blobs {
			if !named[k] {
				s.Delete(k)
			}
		}
	}
	return repaired, nil
}
