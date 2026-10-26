package container

import (
	"sync"
	"time"
)

// Stream numbers, as in the Docker multiplexed stream format.
const (
	Stdout byte = 1
	Stderr byte = 2
)

// Record is one write of a container to stdout or stderr.
type Record struct {
	Stream byte
	Data   []byte
	Time   time.Time
}

// Logs is a container's bounded, append-only output. When it exceeds its
// byte bound the oldest records are dropped, so a chatty container cannot
// exhaust the daemon's memory.
type Logs struct {
	mu      sync.Mutex
	recs    []Record
	base    int // sequence number of recs[0]
	size    int
	max     int
	changed chan struct{}
}

func newLogs(max int) *Logs {
	return &Logs{max: max, changed: make(chan struct{})}
}

// bytes is the output kept.
func (l *Logs) bytes() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return int64(l.size)
}

func (l *Logs) append(stream byte, p []byte) {
	if len(p) == 0 {
		return
	}
	if len(p) > l.max {
		p = p[len(p)-l.max:]
	}
	r := Record{Stream: stream, Data: append([]byte(nil), p...), Time: time.Now()}
	l.mu.Lock()
	l.recs = append(l.recs, r)
	l.size += len(r.Data)
	for l.size > l.max && len(l.recs) > 1 {
		l.size -= len(l.recs[0].Data)
		l.recs[0] = Record{}
		l.recs = l.recs[1:]
		l.base++
	}
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
}

// restore appends a stored record, keeping its time.
func (l *Logs) restore(r Record) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.recs = append(l.recs, r)
	l.size += len(r.Data)
	for l.size > l.max && len(l.recs) > 1 {
		l.size -= len(l.recs[0].Data)
		l.recs = l.recs[1:]
		l.base++
	}
}

// since returns the records from sequence number seq on, the next
// sequence number, and a channel closed at the next append.
func (l *Logs) since(seq int) ([]Record, int, <-chan struct{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if seq < l.base {
		seq = l.base
	}
	out := append([]Record(nil), l.recs[seq-l.base:]...)
	return out, l.base + len(l.recs), l.changed
}

type logWriter struct {
	l      *Logs
	stream byte
}

func (w logWriter) Write(p []byte) (int, error) {
	w.l.append(w.stream, p)
	return len(p), nil
}
