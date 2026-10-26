// Package lineedit is wctr's interactive line editor, as a pure model: key
// events in, the line, the cursor, candidate listings and submitted lines
// out. It has no terminal, DOM or widget type in it, so every front end —
// the playground's xterm.js page, a go-widgets terminal widget — drives the
// same implementation.
//
// It provides: editing (insert, Backspace, Delete, Left/Right, Home/End,
// Ctrl-A/E/B/F, Ctrl-U/K/W, Ctrl-L), history (Up/Down, Ctrl-P/N, bounded,
// exportable for persistence), reverse incremental search (Ctrl-R), and
// Tab completion through cobra's __complete (see Completer): a unique match
// is inserted with a trailing space (unless the directive says NoSpace),
// several are completed to their longest common prefix, a second Tab lists
// them in columns, or as "value  -- description" lines when cobra gives
// descriptions, asking "Display all N possibilities? (y or n)" past a
// threshold. Completion understands the line's quoting.
//
// ParseKeys turns terminal input (what xterm.js's onData delivers) into
// keys; Renderer turns successive States into ANSI output for a terminal.
// A widget can ignore both and use Key and State directly.
package lineedit
