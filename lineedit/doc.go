// Package lineedit moved to github.com/go-widgets/lineedit (go-widgets/
// toolkit#548), so that the wasmcontainers dashboard and wasmdesk share one
// line editor. This module is a shim: every exported name is an alias of
// the one there, so code that still imports this path compiles unchanged
// and gets the same implementation.
//
// Deprecated: import github.com/go-widgets/lineedit.
package lineedit
