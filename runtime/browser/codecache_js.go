//go:build js

package browser

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"syscall/js"
	"time"
)

// CompileStat is one module compilation, for measurements.
type CompileStat struct {
	// Source is where the compiled module came from: "memory" (this
	// engine's own cache), "cache" (WebAssembly.compileStreaming of a
	// Response kept in Cache Storage by an earlier page), "stored" (the
	// same, the first time: it was just stored), or "bytes"
	// (WebAssembly.compile, no Cache Storage).
	Source string
	// Bytes is the module's size; Took how long getting the module took.
	Bytes int
	Took  time.Duration
	// Error says why Cache Storage was not used, if it was not.
	Error string
}

// cacheURL is the same-origin URL a patched module is kept under.
func (e *Executor) cacheURL(patched []byte) string {
	sum := sha256.Sum256(patched)
	return e.opt.CodeCacheURL + hex.EncodeToString(sum[:]) + ".wasm"
}

// compileCached compiles patched through Cache Storage: a Response kept
// there lets the browser reuse the code it compiled for it the last time
// (V8 keys its WebAssembly code cache by the Response's URL; it caches
// only what compileStreaming compiles). What comes back from Cache
// Storage is checked against the module: the cache is the origin's, but
// it is storage, and storage is not trusted with code.
func (e *Executor) compileCached(patched []byte) (js.Value, CompileStat, error) {
	st := CompileStat{Bytes: len(patched)}
	caches := js.Global().Get("caches")
	wa := js.Global().Get("WebAssembly")
	if e.opt.CodeCache == "" || caches.IsUndefined() || wa.Get("compileStreaming").IsUndefined() {
		st.Source = "bytes"
		mod, err := await(wa.Call("compile", uint8Array(patched)))
		return mod, st, err
	}
	url := e.cacheURL(patched)
	want := sha256.Sum256(patched)
	fallback := func(why error) (js.Value, CompileStat, error) {
		st.Source, st.Error = "bytes", why.Error()
		mod, err := await(wa.Call("compile", uint8Array(patched)))
		return mod, st, err
	}
	cache, err := await(caches.Call("open", e.opt.CodeCache))
	if err != nil {
		return fallback(err)
	}
	resp, err := await(cache.Call("match", url))
	if err != nil {
		return fallback(err)
	}
	st.Source = "cache"
	if resp.IsUndefined() {
		st.Source = "stored"
		headers := js.Global().Get("Headers").New()
		headers.Call("set", "Content-Type", "application/wasm")
		init := js.Global().Get("Object").New()
		init.Set("headers", headers)
		r := js.Global().Get("Response").New(uint8Array(patched), init)
		if _, err := await(cache.Call("put", url, r)); err != nil {
			return fallback(fmt.Errorf("storing the module: %v", err)) // QuotaExceededError: compile without it
		}
		if resp, err = await(cache.Call("match", url)); err != nil || resp.IsUndefined() {
			return fallback(fmt.Errorf("the stored module is not there: %v", err))
		}
	}
	// Check the bytes while the browser compiles them.
	check := make(chan bool, 1)
	clone := resp.Call("clone")
	go func() {
		ab, err := await(clone.Call("arrayBuffer"))
		if err != nil {
			check <- false
			return
		}
		b := make([]byte, ab.Get("byteLength").Int())
		js.CopyBytesToGo(b, js.Global().Get("Uint8Array").New(ab))
		check <- sha256.Sum256(b) == want
	}()
	mod, err := await(wa.Call("compileStreaming", resp))
	ok := <-check
	if !ok {
		cache.Call("delete", url)
		return fallback(fmt.Errorf("the module in Cache Storage does not match its digest; deleted"))
	}
	if err != nil {
		cache.Call("delete", url)
		return fallback(fmt.Errorf("compileStreaming: %v", err))
	}
	return mod, st, nil
}

// ClearCodeCache forgets every compiled module, in memory and in Cache
// Storage (after images were removed, or to reclaim space).
func (e *Executor) ClearCodeCache() {
	e.mu.Lock()
	clear(e.compiled)
	e.mu.Unlock()
	if c := js.Global().Get("caches"); e.opt.CodeCache != "" && !c.IsUndefined() {
		c.Call("delete", e.opt.CodeCache)
	}
}
