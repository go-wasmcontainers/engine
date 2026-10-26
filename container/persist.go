package container

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/go-wasmcontainers/engine/rootfs"
	"github.com/go-wasmcontainers/engine/state"
)

// What a container is persisted as: a sealed "container" record (its
// configuration and state), plus content-addressed blobs for its logs and
// its writable layer (rootfs.WriteDiff). Blobs are written before the
// record that names them, and a blob is deleted only once no record names
// it, so whatever a crash interrupts, the stored record and its blobs
// agree: it is the previous checkpoint or the new one.

const (
	recordKind   = "container"
	maxRecord    = 1 << 20
	logsMagic    = "wctr-logs 1\n"
	exitRestored = 137 // as if killed: the engine stopped under it
)

type record struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Created    time.Time         `json:"created"`
	Image      string            `json:"image"`
	ImageID    string            `json:"imageId"`
	Entrypoint []string          `json:"entrypoint,omitempty"`
	Cmd        []string          `json:"cmd,omitempty"`
	Env        []string          `json:"env,omitempty"`
	WorkingDir string            `json:"workingDir,omitempty"`
	Labels     map[string]string `json:"labels,omitempty"`
	Memory     int64             `json:"memory,omitempty"`
	AutoRemove bool              `json:"autoRemove,omitempty"`
	State      State             `json:"state"`
	ExitCode   int               `json:"exitCode"`
	Error      string            `json:"error,omitempty"`
	TimedOut   bool              `json:"timedOut,omitempty"`
	StartedAt  time.Time         `json:"startedAt"`
	FinishedAt time.Time         `json:"finishedAt"`
	Path       string            `json:"path,omitempty"`
	Logs       string            `json:"logs,omitempty"` // blob key
	Diff       string            `json:"diff,omitempty"` // blob key
	Usage      rootfs.Usage      `json:"usage"`
	SavedAt    time.Time         `json:"savedAt"`
}

var (
	idRE   = regexp.MustCompile(`^[0-9a-f]{64}$`)
	blobRE = regexp.MustCompile(`^blob/[0-9a-f]{64}$`)
)

func (r *record) check() error {
	switch {
	case !idRE.MatchString(r.ID):
		return errors.New("bad id")
	case !nameRE.MatchString(r.Name) || len(r.Name) > 128:
		return errors.New("bad name")
	case r.State != Created && r.State != Running && r.State != Exited:
		return fmt.Errorf("bad state %q", r.State)
	case r.Logs != "" && !blobRE.MatchString(r.Logs), r.Diff != "" && !blobRE.MatchString(r.Diff):
		return errors.New("bad blob key")
	case len(r.Entrypoint)+len(r.Cmd) == 0:
		return errors.New("no command")
	}
	return nil
}

// DecodeRecord reads a stored container record; it is exported for the
// fuzzer and for tools that inspect a data root.
func DecodeRecord(v []byte) (*record, error) {
	p, err := state.Unseal(recordKind, v, maxRecord)
	if err != nil {
		return nil, err
	}
	var r record
	if err := json.Unmarshal(p, &r); err != nil {
		return nil, fmt.Errorf("%w: %v", state.ErrCorrupt, err)
	}
	if err := r.check(); err != nil {
		return nil, fmt.Errorf("%w: %v", state.ErrCorrupt, err)
	}
	return &r, nil
}

func encodeLogs(recs []Record) []byte {
	var b bytes.Buffer
	b.WriteString(logsMagic)
	var h [13]byte
	for _, r := range recs {
		h[0] = r.Stream
		binary.BigEndian.PutUint64(h[1:9], uint64(r.Time.UnixNano()))
		binary.BigEndian.PutUint32(h[9:13], uint32(len(r.Data)))
		b.Write(h[:])
		b.Write(r.Data)
	}
	return b.Bytes()
}

func decodeLogs(b []byte, max int) ([]Record, error) {
	if !bytes.HasPrefix(b, []byte(logsMagic)) {
		return nil, fmt.Errorf("%w: not a log", state.ErrCorrupt)
	}
	b = b[len(logsMagic):]
	var out []Record
	total := 0
	for len(b) > 0 {
		if len(b) < 13 {
			return nil, fmt.Errorf("%w: truncated log", state.ErrCorrupt)
		}
		s := b[0]
		t := int64(binary.BigEndian.Uint64(b[1:9]))
		n := int(binary.BigEndian.Uint32(b[9:13]))
		b = b[13:]
		if (s != Stdout && s != Stderr) || n > len(b) || n == 0 {
			return nil, fmt.Errorf("%w: bad log record", state.ErrCorrupt)
		}
		total += n
		if total > max {
			return nil, fmt.Errorf("%w: log over %d bytes", state.ErrCorrupt, max)
		}
		out = append(out, Record{Stream: s, Time: time.Unix(0, t), Data: append([]byte(nil), b[:n]...)})
		b = b[n:]
	}
	return out, nil
}

// persisted is what was last stored for a container (guarded by
// Manager.pmu).
type persisted struct {
	logsKey, diffKey string
	logsSeq          int
	diffChanges      uint64
	saved            bool
}

// schedule asks the writer to store c's current state (or to delete it,
// once it is removed). Many changes before the writer gets to it are one
// write: only the latest state matters.
func (m *Manager) schedule(c *Container) {
	if m.opt.Store == nil {
		return
	}
	m.qmu.Lock()
	if !c.queued {
		c.queued = true
		m.queue = append(m.queue, c)
	}
	m.qmu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
}

// persister is the one writer: commands never wait on storage (an
// IndexedDB transaction, an fsync), and writes stay in order.
func (m *Manager) persister() {
	for range m.wake {
		for {
			m.qmu.Lock()
			batch := m.queue
			m.queue = nil
			for _, c := range batch {
				c.queued = false
			}
			if len(batch) == 0 {
				for _, f := range m.flushers {
					close(f)
				}
				m.flushers = nil
				m.qmu.Unlock()
				break
			}
			m.qmu.Unlock()
			for _, c := range batch {
				m.checkpoint(c)
			}
		}
	}
}

// Flush waits until every change made so far is stored.
func (m *Manager) Flush(ctx context.Context) error {
	if m.opt.Store == nil {
		return nil
	}
	f := make(chan struct{})
	m.qmu.Lock()
	m.flushers = append(m.flushers, f)
	m.qmu.Unlock()
	select {
	case m.wake <- struct{}{}:
	default:
	}
	select {
	case <-f:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// checkpoint stores c — blobs that changed, then its record — or deletes
// it if it was removed. A failure (a full store) leaves the previous
// checkpoint in place and is reported by PersistError.
func (m *Manager) checkpoint(c *Container) {
	m.mu.Lock()
	live := m.byID[c.id] == c
	m.mu.Unlock()
	if !live {
		m.forget(c)
		return
	}
	m.pmu.Lock()
	defer m.pmu.Unlock()
	m.setPersistErr(m.checkpointLocked(c))
}

func (m *Manager) setPersistErr(err error) {
	m.mu.Lock()
	m.persistErr = err
	m.mu.Unlock()
}

// PersistError is the last checkpoint's failure, nil when the store holds
// every container's latest state.
func (m *Manager) PersistError() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.persistErr
}

func (m *Manager) checkpointLocked(c *Container) error {
	s := m.opt.Store
	p := &c.persisted
	recs, seq, _ := c.logs.since(0)
	logsKey := p.logsKey
	if seq != p.logsSeq || (logsKey == "" && len(recs) > 0) {
		logsKey = ""
		if len(recs) > 0 {
			b := encodeLogs(recs)
			logsKey = state.BlobKey(b)
			if err := s.Put(logsKey, b); err != nil {
				return err
			}
		}
	}
	diffKey := p.diffKey
	changes := c.fs.Changes()
	var usage rootfs.Usage
	if changes != p.diffChanges || !p.saved {
		var buf bytes.Buffer
		u, err := c.fs.WriteDiff(&buf)
		if err != nil {
			return err
		}
		usage = u
		diffKey = ""
		if buf.Len() > 1024 { // more than the end-of-archive blocks
			diffKey = state.BlobKey(buf.Bytes())
			if err := s.Put(diffKey, buf.Bytes()); err != nil {
				return err
			}
		}
	} else {
		b, n := c.fs.Usage()
		usage = rootfs.Usage{Bytes: b, Files: n}
	}
	sn := c.Snapshot()
	c.mu.Lock()
	r := record{
		ID: c.id, Name: c.name, Created: c.created, Image: c.cfg.Image, ImageID: c.cfg.ImageID,
		Entrypoint: c.cfg.Entrypoint, Cmd: c.cfg.Cmd, Env: c.cfg.Env, WorkingDir: c.cfg.WorkingDir,
		Labels: c.cfg.Labels, Memory: c.cfg.MemoryLimitBytes, AutoRemove: c.cfg.AutoRemove,
		State: sn.State, ExitCode: sn.ExitCode, Error: sn.Error, TimedOut: sn.TimedOut,
		StartedAt: sn.StartedAt, FinishedAt: sn.FinishedAt, Path: sn.Path,
		Logs: logsKey, Diff: diffKey, Usage: usage, SavedAt: time.Now().UTC(),
	}
	c.mu.Unlock()
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if err := s.Put(recordKind+"/"+c.id, state.Seal(recordKind, raw)); err != nil {
		return err
	}
	oldLogs, oldDiff := p.logsKey, p.diffKey
	*p = persisted{logsKey: logsKey, diffKey: diffKey, logsSeq: seq, diffChanges: changes, saved: true}
	for _, k := range []string{oldLogs, oldDiff} {
		if k != "" && k != logsKey && k != diffKey {
			m.dropBlobLocked(k)
		}
	}
	return nil
}

// dropBlobLocked deletes a blob unless another container still names it
// (content addressing shares identical logs or layers).
func (m *Manager) dropBlobLocked(k string) {
	m.mu.Lock()
	for _, o := range m.byID {
		if o.persisted.logsKey == k || o.persisted.diffKey == k {
			m.mu.Unlock()
			return
		}
	}
	m.mu.Unlock()
	m.opt.Store.Delete(k)
}

// forget deletes c's record and blobs; c is already out of the maps.
func (m *Manager) forget(c *Container) {
	if m.opt.Store == nil {
		return
	}
	m.pmu.Lock()
	defer m.pmu.Unlock()
	if err := m.opt.Store.Delete(recordKind + "/" + c.id); err != nil {
		m.setPersistErr(err)
		return
	}
	for _, k := range []string{c.persisted.logsKey, c.persisted.diffKey} {
		if k != "" {
			m.dropBlobLocked(k)
		}
	}
	c.persisted = persisted{}
}

// checkpointWhileRunning stores c every opt.CheckpointEvery while it runs
// and anything changed, so a reload keeps what it wrote until shortly
// before.
func (m *Manager) checkpointWhileRunning(c *Container) {
	if m.opt.Store == nil || m.opt.CheckpointEvery < 0 {
		return
	}
	every := m.opt.CheckpointEvery
	if every == 0 {
		every = 2 * time.Second
	}
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		c.mu.Lock()
		running, ch := c.state == Running, c.changed
		c.mu.Unlock()
		if !running {
			return
		}
		select {
		case <-t.C:
			m.pmu.Lock()
			_, seq, _ := c.logs.since(0)
			dirty := seq != c.persisted.logsSeq || c.fs.Changes() != c.persisted.diffChanges
			m.pmu.Unlock()
			if dirty {
				m.schedule(c)
			}
		case <-ch:
		}
	}
}

// RestoreReport says what Restore did.
type RestoreReport struct {
	// Restored containers, of which Stopped were running when the engine
	// stopped and are now exited with status 137.
	Restored, Stopped int
	// Removed are --rm containers that had run, removed as they would
	// have been.
	Removed []string
	// Repaired describes what was found corrupted and dropped.
	Repaired []string
}

// Restore loads every stored container. lower returns the frozen root
// filesystem of an image by ID; lim bounds reading a stored writable layer.
// A record that fails its seal or its checks is deleted and reported; a
// blob that does, is dropped (the container keeps its configuration and
// loses its logs or its writes), and so is a container whose image is gone.
func (m *Manager) Restore(lower func(imageID string) (*rootfs.FS, error), lim rootfs.Limits) (RestoreReport, error) {
	var rep RestoreReport
	s := m.opt.Store
	if s == nil {
		return rep, nil
	}
	m.pmu.Lock()
	defer m.pmu.Unlock()
	keys, err := s.Keys(recordKind + "/")
	if err != nil {
		return rep, err
	}
	repair := func(format string, a ...any) { rep.Repaired = append(rep.Repaired, fmt.Sprintf(format, a...)) }
	var autoRemove []*Container
	for _, k := range keys {
		v, err := s.Get(k)
		if errors.Is(err, state.ErrNotFound) {
			continue // deleted since it was listed
		}
		if err != nil {
			return rep, err
		}
		r, err := DecodeRecord(v)
		if err == nil && k != recordKind+"/"+r.ID {
			err = errors.New("record stored under another key")
		}
		if err != nil {
			repair("%s: %v; dropped", k, err)
			s.Delete(k)
			continue
		}
		low, err := lower(r.ImageID)
		if err != nil {
			repair("container %s (%s): image %s: %v; dropped", r.ID[:12], r.Name, shortID(r.ImageID), err)
			s.Delete(k)
			continue
		}
		c := &Container{
			id: r.ID, name: r.Name, created: r.Created,
			cfg: Config{Image: r.Image, ImageID: r.ImageID, Entrypoint: r.Entrypoint, Cmd: r.Cmd, Env: r.Env,
				WorkingDir: r.WorkingDir, Labels: r.Labels, MemoryLimitBytes: r.Memory, AutoRemove: r.AutoRemove},
			fs: low.Clone(m.opt.Quota), logs: newLogs(m.opt.MaxLogBytes),
			state: r.State, exitCode: r.ExitCode, errMsg: r.Error, timedOut: r.TimedOut,
			startedAt: r.StartedAt, finishedAt: r.FinishedAt, path: r.Path, changed: make(chan struct{}),
		}
		fixed := false
		if r.Diff != "" {
			b, err := state.GetBlob(s, r.Diff, int(min(lim.MaxBytes+lim.MaxBytes/4+1<<20, 1<<30)))
			if err == nil {
				err = c.fs.ApplyDiff(bytes.NewReader(b), r.Usage, lim)
			}
			if err != nil {
				repair("container %s (%s): writable layer: %v; its writes are lost", r.ID[:12], r.Name, err)
				c.fs = low.Clone(m.opt.Quota)
				r.Diff, fixed = "", true
			}
		}
		if r.Logs != "" {
			b, err := state.GetBlob(s, r.Logs, m.opt.MaxLogBytes+m.opt.MaxLogBytes/4+1<<16)
			var recs []Record
			if err == nil {
				recs, err = decodeLogs(b, m.opt.MaxLogBytes)
			}
			if err != nil {
				repair("container %s (%s): logs: %v; its logs are lost", r.ID[:12], r.Name, err)
				r.Logs, fixed = "", true
			}
			for _, rec := range recs {
				c.logs.restore(rec)
			}
		}
		if c.state == Running {
			// It was running when the engine stopped: it is not any more.
			c.state, c.exitCode, c.errMsg = Exited, exitRestored, "the engine stopped while it was running"
			c.finishedAt = r.SavedAt
			c.exits = 1
			rep.Stopped++
		}
		_, seq, _ := c.logs.since(0)
		c.persisted = persisted{logsKey: r.Logs, diffKey: r.Diff, logsSeq: seq, diffChanges: c.fs.Changes(), saved: true}
		m.mu.Lock()
		if m.byName[c.name] != nil || m.byID[c.id] != nil || len(m.byID) >= m.opt.MaxContainers {
			m.mu.Unlock()
			repair("container %s (%s): duplicate or over the limit; dropped", r.ID[:12], r.Name)
			s.Delete(k)
			continue
		}
		m.byID[c.id] = c
		m.byName[c.name] = c
		m.mu.Unlock()
		rep.Restored++
		if c.cfg.AutoRemove && c.state == Exited {
			autoRemove = append(autoRemove, c)
			continue
		}
		if c.state != r.State || fixed {
			if err := m.checkpointLocked(c); err != nil { // store the stopped or repaired state
				m.persistErr = err
			}
		}
	}
	// Blobs no record names: left by a crash between a blob and its record.
	if blobs, err := s.Keys("blob/"); err == nil {
		for _, k := range blobs {
			m.dropBlobLocked(k)
		}
	}
	m.pmu.Unlock()
	for _, c := range autoRemove {
		m.removeNow(c)
		rep.Removed = append(rep.Removed, c.id)
	}
	m.pmu.Lock()
	return rep, nil
}

func shortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
