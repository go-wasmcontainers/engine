// Package lineedit is a line editor as a pure model: key events in, the
// line, the cursor, candidate listings and submitted lines out. It has no
// terminal, DOM or widget type in it, so every front end — the playground's
// xterm.js page, a go-widgets terminal widget, wctr's terminal, the guest
// shell's sh — drives the same implementation. It imports the standard
// library only (and no net/http), so a WebAssembly guest can link it
// without growing: it is its own module, github.com/go-wasmcontainers/
// engine/lineedit, versioned by tags lineedit/vX.Y.Z.
//
// It provides: editing (insert, Backspace, Delete, Left/Right, Home/End,
// Ctrl-A/E/B/F, Ctrl-U/K/W, Ctrl-L), history (Up/Down, Ctrl-P/N, bounded,
// exportable for persistence), reverse incremental search (Ctrl-R), and
// Tab completion through a Completer: a unique match is inserted with a
// trailing space (unless the completer says noSpace), several are completed
// to their longest common prefix, a second Tab lists them in columns, or as
// "value  -- description" lines when candidates have descriptions, asking
// "Display all N possibilities? (y or n)" past a threshold. Completion
// understands the line's quoting. Columns are counted with RuneWidth, so
// wide (CJK, emoji) and combining characters place the cursor correctly.
//
// ParseKeys turns terminal input (what xterm.js's onData delivers) into
// keys; Renderer turns successive States into ANSI output for a terminal.
// A widget can ignore both and use Key and State directly. Reader is the
// ready-made driver for a raw-mode terminal stream: it reads bytes, decodes
// keys, edits and draws, and returns one line per ReadLine.
package lineedit
