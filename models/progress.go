package models

import "context"

// ProgressFunc is told how many bytes of a model are stored so far, and the
// total (Entry.Size), while a download runs. It is called from the
// downloading goroutine, never concurrently, at most once per chunk
// (HashChunk) and once at the end (done == total).
type ProgressFunc func(e Entry, done, total int64)

type progressKey struct{}

// WithProgress returns a context whose downloads (OpenOPFS) report to fn.
func WithProgress(ctx context.Context, fn ProgressFunc) context.Context {
	return context.WithValue(ctx, progressKey{}, fn)
}

// ReportProgress calls the ProgressFunc of ctx, if any.
func ReportProgress(ctx context.Context, e Entry, done, total int64) {
	if fn, ok := ctx.Value(progressKey{}).(ProgressFunc); ok && fn != nil {
		fn(e, done, total)
	}
}

// Cached is one model file of the browser cache.
type Cached struct {
	Name   string // the registry name, or "" for a file the registry does not list
	File   string // the file in the cache: SHA256.gguf
	Size   int64
	Pinned bool // the registry lists it
}
