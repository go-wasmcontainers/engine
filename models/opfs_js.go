//go:build js

package models

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"syscall/js"
)

// The browser model cache: the Origin Private File System, read through
// FileSystemSyncAccessHandle. A guest's fd_read runs inside a syscall/js
// callback, where Go cannot wait for the network, so a model is first
// brought into OPFS (OpenOPFS, outside any callback: ranges from the pinned
// URL, verified whole) and then read synchronously by ranges. The origin's
// storage quota covers it (docs/llm-design.md, "Where the weights live").

const opfsDir = "go-wasmcontainers-models"

type opfsResult struct {
	v   js.Value
	err error
}

// await waits for a promise. It must not be called from inside a js.Func
// callback (the event loop that settles the promise could not run).
func await(p js.Value) (js.Value, error) {
	ch := make(chan opfsResult, 1)
	then := js.FuncOf(func(_ js.Value, a []js.Value) any {
		var v js.Value
		if len(a) > 0 {
			v = a[0]
		}
		ch <- opfsResult{v: v}
		return nil
	})
	fail := js.FuncOf(func(_ js.Value, a []js.Value) any {
		msg := "rejected"
		if len(a) > 0 && a[0].Type() == js.TypeObject {
			msg = a[0].Get("name").String() + ": " + a[0].Get("message").String()
		}
		ch <- opfsResult{err: errors.New("opfs: " + msg)}
		return nil
	})
	defer then.Release()
	defer fail.Release()
	p.Call("then", then, fail)
	r := <-ch
	return r.v, r.err
}

type opfsSource struct {
	name   string
	size   int64
	handle js.Value
	mu     sync.Mutex
	closed bool
}

func (s *opfsSource) Size() int64 { return s.size }

// ReadAt reads synchronously: a sync access handle's read.
func (s *opfsSource) ReadAt(p []byte, off int64) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, errors.New("opfs: closed")
	}
	if off < 0 {
		return 0, errors.New("opfs: negative offset")
	}
	if off >= s.size {
		return 0, io.EOF
	}
	want := int(min(int64(len(p)), s.size-off))
	arr := js.Global().Get("Uint8Array").New(want)
	opts := js.Global().Get("Object").New()
	opts.Set("at", float64(off))
	n := s.handle.Call("read", arr, opts).Int()
	js.CopyBytesToGo(p[:n], arr)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Close releases nothing: the handle is shared by every container of the
// page (a sync access handle is exclusive), and lives as long as the page.
func (s *opfsSource) Close() error { return nil }

var (
	opfsMu    sync.Mutex
	opfsOpen  = map[string]*opfsSource{}
	opfsReady = map[string]bool{} // verified in this page
)

// OpenOPFS returns the verified source of entry e from the origin's OPFS
// cache, filling it first when it is absent, of the wrong size or corrupted:
// remote supplies the ranged source of the pinned URL (an HTTPSource). A
// second call, from another container, shares the handle and the earlier
// verification. It must be called outside a syscall/js callback.
func OpenOPFS(ctx context.Context, e Entry, remote func() (Source, error)) (Source, error) {
	if err := e.Check(); err != nil {
		return nil, err
	}
	name := e.SHA256 + ".gguf"
	opfsMu.Lock()
	defer opfsMu.Unlock()
	if s, ok := opfsOpen[name]; ok && opfsReady[name] {
		return s, nil
	}
	nav := js.Global().Get("navigator").Get("storage")
	if !nav.Truthy() || !nav.Get("getDirectory").Truthy() {
		return nil, errors.New("opfs: not available in this browser context")
	}
	root, err := await(nav.Call("getDirectory"))
	if err != nil {
		return nil, err
	}
	co := js.Global().Get("Object").New()
	co.Set("create", true)
	dir, err := await(root.Call("getDirectoryHandle", opfsDir, co))
	if err != nil {
		return nil, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		s, err := openHandle(dir, name, e, co)
		if err == nil {
			if err = Verify(s, e); err == nil {
				opfsOpen[name], opfsReady[name] = s, true
				return s, nil
			}
		}
		// Absent, truncated or corrupted: drop it and fetch again (once).
		if s != nil {
			s.handle.Call("close")
			delete(opfsOpen, name)
		}
		if attempt == 1 {
			return nil, fmt.Errorf("opfs: %s still fails after a fresh download: %w", e.Name, err)
		}
		if _, rerr := await(dir.Call("removeEntry", name)); rerr != nil && !isNotFound(rerr) {
			return nil, rerr
		}
		if err := fill(ctx, dir, name, e, co, remote); err != nil {
			return nil, err
		}
	}
	return nil, errors.New("opfs: unreachable")
}

func isNotFound(err error) bool {
	return err != nil && len(err.Error()) >= 14 && err.Error()[:14] == "opfs: NotFound"
}

func openHandle(dir js.Value, name string, e Entry, co js.Value) (*opfsSource, error) {
	fh, err := await(dir.Call("getFileHandle", name, co))
	if err != nil {
		return nil, err
	}
	ah, err := await(fh.Call("createSyncAccessHandle"))
	if err != nil {
		return nil, err
	}
	size := int64(ah.Call("getSize").Float())
	s := &opfsSource{name: name, size: size, handle: ah}
	if size != e.Size {
		ah.Call("close")
		return nil, fmt.Errorf("%w: cached file is %d bytes, expected %d", ErrSize, size, e.Size)
	}
	return s, nil
}

// fill downloads the model into the OPFS file by 16 MiB ranges.
func fill(ctx context.Context, dir js.Value, name string, e Entry, co js.Value, remote func() (Source, error)) error {
	src, err := remote()
	if err != nil {
		return err
	}
	defer src.Close()
	if src.Size() != e.Size {
		return fmt.Errorf("%w: remote source of %d bytes, expected %d", ErrSize, src.Size(), e.Size)
	}
	fh, err := await(dir.Call("getFileHandle", name, co))
	if err != nil {
		return err
	}
	ah, err := await(fh.Call("createSyncAccessHandle"))
	if err != nil {
		return err
	}
	defer ah.Call("close")
	ah.Call("truncate", 0)
	buf := make([]byte, min(int64(HashChunk), e.Size))
	for off := int64(0); off < e.Size; {
		if err := ctx.Err(); err != nil {
			return err
		}
		n := int(min(int64(len(buf)), e.Size-off))
		if _, err := src.ReadAt(buf[:n], off); err != nil && !(err == io.EOF) {
			return fmt.Errorf("opfs: downloading %s at %d: %w", e.Name, off, err)
		}
		arr := js.Global().Get("Uint8Array").New(n)
		js.CopyBytesToJS(arr, buf[:n])
		opts := js.Global().Get("Object").New()
		opts.Set("at", float64(off))
		if w := ah.Call("write", arr, opts).Int(); w != n {
			return fmt.Errorf("opfs: wrote %d of %d bytes (storage quota?)", w, n)
		}
		off += int64(n)
	}
	ah.Call("flush")
	return nil
}
