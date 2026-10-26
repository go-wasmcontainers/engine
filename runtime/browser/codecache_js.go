//go:build js

package browser

import (
	"bytes"
	"fmt"
	"syscall/js"
	"time"
)

// CompileStat is one module compilation, for measurements.
type CompileStat struct {
	// Source is where the compiled module came from: "memory" (this
	// engine's own cache), "cache" (WebAssembly.compileStreaming of a
	// Response kept in Cache Storage by an earlier page), "stored"
	// (WebAssembly.compile, the module being stored for the next page), or
	// "bytes" (WebAssembly.compile, no Cache Storage).
	Source string
	// Bytes is the module's size; Took how long getting the module took,
	// of which Compile in the browser's compile call alone.
	Bytes   int
	Took    time.Duration
	Compile time.Duration
	// Error says why Cache Storage was not used, if it was not.
	Error string
}

// cacheURL is the same-origin URL a patched module is kept under: the
// image module's digest (already computed for the in-memory cache) and
// the memory limit, which determine the patched bytes.
func (e *Executor) cacheURL(key cacheKey) string {
	return fmt.Sprintf("%s%s-%d.wasm", e.opt.CodeCacheURL, key.digest.Encoded(), key.pages)
}

// compileCached compiles patched through Cache Storage: a Response kept
// there lets the browser reuse the code it compiled for it the last time
// (V8 keys its WebAssembly code cache by the Response's URL; it caches
// only what compileStreaming compiles). What comes back from Cache
// Storage is checked against the module: the cache is the origin's, but
// it is storage, and storage is not trusted with code.
func (e *Executor) compileCached(key cacheKey, patched []byte) (js.Value, CompileStat, error) {
	st := CompileStat{Bytes: len(patched)}
	caches := js.Global().Get("caches")
	wa := js.Global().Get("WebAssembly")
	fromBytes := func() (js.Value, CompileStat, error) {
		st.Source = "bytes"
		u := uint8Array(patched)
		t0 := time.Now()
		mod, err := await(wa.Call("compile", u))
		st.Compile = time.Since(t0)
		return mod, st, err
	}
	if e.opt.CodeCache == "" || caches.IsUndefined() || wa.Get("compileStreaming").IsUndefined() {
		return fromBytes()
	}
	url := e.cacheURL(key)
	fallback := func(why error) (js.Value, CompileStat, error) {
		st.Error = why.Error()
		return fromBytes()
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
		// Not there yet: compile now, store for the next page load in the
		// background (a full quota only means no cache next time).
		headers := js.Global().Get("Headers").New()
		headers.Call("set", "Content-Type", "application/wasm")
		init := js.Global().Get("Object").New()
		init.Set("headers", headers)
		r := js.Global().Get("Response").New(uint8Array(patched), init)
		go await(cache.Call("put", url, r))
		mod, st, err := fromBytes()
		st.Source = "stored"
		return mod, st, err
	}
	// Check the bytes while the browser compiles them: they must be the
	// module this engine derives from the image (whose digest the image
	// store verified), byte for byte.
	check := make(chan bool, 1)
	clone := resp.Call("clone")
	go func() {
		ab, err := await(clone.Call("arrayBuffer"))
		if err != nil || ab.Get("byteLength").Int() != len(patched) {
			check <- false
			return
		}
		b := make([]byte, len(patched))
		js.CopyBytesToGo(b, js.Global().Get("Uint8Array").New(ab))
		check <- bytes.Equal(b, patched)
	}()
	t0 := time.Now()
	mod, err := await(wa.Call("compileStreaming", resp))
	st.Compile = time.Since(t0)
	ok := <-check
	if !ok {
		cache.Call("delete", url)
		return fallback(fmt.Errorf("the module in Cache Storage is not the image's; deleted"))
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
