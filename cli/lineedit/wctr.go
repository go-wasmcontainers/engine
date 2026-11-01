// Package lineedit is wctr's interactive line editor: a thin adapter over
// the standard-library-only core, github.com/go-wasmcontainers/engine/
// lineedit (see its documentation for the editor, ParseKeys, Renderer and
// Reader), plus WctrCompleter, which completes through cobra's __complete
// and so needs engine/cli. Everything the package exported before the core
// was split out is re-exported here as an alias, so the playground and the
// dashboard compile unchanged.
//
// Code that must not link engine/cli (and so net/http) — the guest shell —
// imports the core directly.
package lineedit

import (
	"context"
	"strings"

	"github.com/go-wasmcontainers/engine/cli"
	core "github.com/go-wasmcontainers/engine/lineedit"
)

type (
	// Options configure an Editor.
	Options = core.Options
	// Event is what a key did besides editing.
	Event = core.Event
	// State is what to display.
	State = core.State
	// Editor is the line editor.
	Editor = core.Editor
	// KeyCode names a non-character key.
	KeyCode = core.KeyCode
	// Key is one key press.
	Key = core.Key
	// Renderer turns States into ANSI output for a terminal.
	Renderer = core.Renderer
	// Reader edits lines read from a raw-mode terminal stream.
	Reader = core.Reader
	// Completer lists the candidates for the word being typed.
	Completer = core.Completer
	// Partial is the word being completed, split from its line.
	Partial = core.Partial
)

// The keys.
const (
	KeyRune      = core.KeyRune
	KeyEnter     = core.KeyEnter
	KeyBackspace = core.KeyBackspace
	KeyDelete    = core.KeyDelete
	KeyLeft      = core.KeyLeft
	KeyRight     = core.KeyRight
	KeyHome      = core.KeyHome
	KeyEnd       = core.KeyEnd
	KeyUp        = core.KeyUp
	KeyDown      = core.KeyDown
	KeyTab       = core.KeyTab
	KeyEscape    = core.KeyEscape
	KeyCtrlA     = core.KeyCtrlA
	KeyCtrlB     = core.KeyCtrlB
	KeyCtrlC     = core.KeyCtrlC
	KeyCtrlD     = core.KeyCtrlD
	KeyCtrlE     = core.KeyCtrlE
	KeyCtrlF     = core.KeyCtrlF
	KeyCtrlG     = core.KeyCtrlG
	KeyCtrlK     = core.KeyCtrlK
	KeyCtrlL     = core.KeyCtrlL
	KeyCtrlN     = core.KeyCtrlN
	KeyCtrlP     = core.KeyCtrlP
	KeyCtrlR     = core.KeyCtrlR
	KeyCtrlU     = core.KeyCtrlU
	KeyCtrlW     = core.KeyCtrlW
	KeyUnknown   = core.KeyUnknown

	// MaxLine bounds the line being edited, in runes.
	MaxLine = core.MaxLine
)

// ErrUnterminated and ErrInterrupt are the core's errors.
var (
	ErrUnterminated = core.ErrUnterminated
	ErrInterrupt    = core.ErrInterrupt
)

var (
	// New returns an editor.
	New = core.New
	// R is a character key.
	R = core.R
	// ParseKeys turns terminal input into keys.
	ParseKeys = core.ParseKeys
	// Format lays candidates out for a terminal.
	Format = core.Format
	// Split splits a line into words, shell-style.
	Split = core.Split
	// SplitPartial splits a line prefix into the words before the one being
	// typed and that word.
	SplitPartial = core.SplitPartial
	// Quote quotes a completion for the line's quoting.
	Quote = core.Quote
)

// WctrCompleter completes a line whose first word is "wctr" through the
// command tree o describes (cobra's __complete, in-process), and the first
// word itself from builtins (which should include "wctr").
func WctrCompleter(o cli.Options, builtins []cli.Candidate) Completer {
	return func(ctx context.Context, words []string, current string) ([]cli.Candidate, bool) {
		if len(words) == 0 {
			var out []cli.Candidate
			for _, b := range builtins {
				if strings.HasPrefix(b.Value, current) {
					out = append(out, b)
				}
			}
			return out, false
		}
		if words[0] != "wctr" {
			return nil, false
		}
		c, err := cli.Complete(ctx, o, append(append([]string{}, words[1:]...), current))
		if err != nil || c.Failed() {
			return nil, false
		}
		return c.Candidates, c.NoSpace()
	}
}
