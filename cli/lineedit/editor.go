package lineedit

import (
	"context"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/go-wasmcontainers/engine/cli"
)

// Options configure an Editor.
type Options struct {
	// Prompt is shown before the line (default "$ ").
	Prompt string
	// Complete answers Tab; nil disables completion.
	Complete Completer
	// HistorySize bounds the history (default 500).
	HistorySize int
	// Cols is the display width used for listings (default 80); SetCols
	// changes it.
	Cols int
}

// Event is what a key did besides editing.
type Event struct {
	// Submit is set when Enter ended a line; Line is that line.
	Submit bool
	Line   string
	// Print are lines to show above the prompt (a candidate listing, a
	// question); the line is then shown again below them.
	Print []string
	// Interrupt is Ctrl-C on an empty or abandoned line ("^C").
	Interrupt bool
	// Clear is Ctrl-L: clear the screen, keep the line.
	Clear bool
	// EOF is Ctrl-D on an empty line.
	EOF bool
	// Bell is a key that did nothing (no completion, end of history).
	Bell bool
}

// State is what to display.
type State struct {
	Prompt string
	Line   string
	// Cursor is the cursor's position in Line, in runes.
	Cursor int
}

type mode int

const (
	modeEdit mode = iota
	modeSearch
	modeAskAll
)

// Editor is the line editor.
type Editor struct {
	opt     Options
	line    []rune
	cur     int
	history []string
	hist    int    // index into history while browsing; len(history) = the new line
	saved   []rune // the new line, kept while browsing history
	lastTab bool   // the previous key was a Tab that left candidates
	pending []cli.Candidate
	mode    mode
	query   []rune // Ctrl-R
	match   int    // history index of the current search match, -1 none
}

// New returns an editor.
func New(o Options) *Editor {
	if o.Prompt == "" {
		o.Prompt = "$ "
	}
	if o.HistorySize <= 0 {
		o.HistorySize = 500
	}
	if o.Cols <= 0 {
		o.Cols = 80
	}
	return &Editor{opt: o, match: -1}
}

// SetCols sets the display width for listings.
func (e *Editor) SetCols(n int) {
	if n > 0 {
		e.opt.Cols = n
	}
}

// History returns the history, oldest first (to persist it).
func (e *Editor) History() []string { return append([]string(nil), e.history...) }

// SetHistory replaces the history (a restored one), bounded. A restored
// history is untrusted (browser storage, a file): an entry that a user
// could not have typed — invalid UTF-8 or a non-printable rune, which the
// renderer would otherwise write to the terminal as is — is dropped.
func (e *Editor) SetHistory(h []string) {
	e.history = nil
	for _, l := range h {
		if typable(l) {
			e.addHistory(l)
		}
	}
	e.hist = len(e.history)
}

// typable reports whether the editor could have produced l.
func typable(l string) bool {
	if !utf8.ValidString(l) {
		return false
	}
	for _, r := range l {
		if r != ' ' && !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func (e *Editor) addHistory(l string) {
	if strings.TrimSpace(l) == "" || len(l) > MaxLine {
		return
	}
	if n := len(e.history); n > 0 && e.history[n-1] == l {
		return
	}
	e.history = append(e.history, l)
	if len(e.history) > e.opt.HistorySize {
		e.history = e.history[len(e.history)-e.opt.HistorySize:]
	}
}

// State returns what to display.
func (e *Editor) State() State {
	if e.mode == modeSearch {
		found := ""
		if e.match >= 0 {
			found = e.history[e.match]
		}
		prompt := "(reverse-i-search)`" + string(e.query) + "': "
		return State{Prompt: prompt, Line: found, Cursor: len([]rune(found))}
	}
	return State{Prompt: e.opt.Prompt, Line: string(e.line), Cursor: e.cur}
}

// Reset clears the line (a new prompt).
func (e *Editor) Reset() {
	e.line, e.cur, e.lastTab, e.pending, e.mode = nil, 0, false, nil, modeEdit
	e.hist = len(e.history)
}

func (e *Editor) insert(rs ...rune) {
	if len(e.line)+len(rs) > MaxLine {
		return
	}
	e.line = append(e.line[:e.cur], append(append([]rune(nil), rs...), e.line[e.cur:]...)...)
	e.cur += len(rs)
}

func (e *Editor) setLine(s string) {
	e.line = []rune(s)
	e.cur = len(e.line)
}

// Key applies one key; ctx bounds a completion.
func (e *Editor) Key(ctx context.Context, k Key) Event {
	switch e.mode {
	case modeSearch:
		return e.searchKey(ctx, k)
	case modeAskAll:
		e.mode = modeEdit
		cands := e.pending
		e.pending = nil
		if k.Code == KeyRune && (k.Rune == 'y' || k.Rune == 'Y') {
			return Event{Print: Format(cands, e.opt.Cols)}
		}
		return Event{Print: []string{}}
	}
	tab := k.Code == KeyTab
	defer func() {
		if !tab {
			e.lastTab = false
		}
	}()
	switch k.Code {
	case KeyRune:
		if !unicode.IsPrint(k.Rune) && k.Rune != ' ' {
			return Event{}
		}
		e.insert(k.Rune)
	case KeyEnter:
		l := string(e.line)
		e.addHistory(l)
		e.Reset()
		return Event{Submit: true, Line: l}
	case KeyBackspace:
		if e.cur == 0 {
			return Event{Bell: true}
		}
		e.line = append(e.line[:e.cur-1], e.line[e.cur:]...)
		e.cur--
	case KeyDelete:
		if e.cur == len(e.line) {
			return Event{Bell: true}
		}
		e.line = append(e.line[:e.cur], e.line[e.cur+1:]...)
	case KeyCtrlD:
		if len(e.line) == 0 {
			return Event{EOF: true}
		}
		if e.cur < len(e.line) {
			e.line = append(e.line[:e.cur], e.line[e.cur+1:]...)
		}
	case KeyLeft, KeyCtrlB:
		if e.cur > 0 {
			e.cur--
		}
	case KeyRight, KeyCtrlF:
		if e.cur < len(e.line) {
			e.cur++
		}
	case KeyHome, KeyCtrlA:
		e.cur = 0
	case KeyEnd, KeyCtrlE:
		e.cur = len(e.line)
	case KeyCtrlU:
		e.line = append([]rune(nil), e.line[e.cur:]...)
		e.cur = 0
	case KeyCtrlK:
		e.line = e.line[:e.cur]
	case KeyCtrlW:
		i := e.cur
		for i > 0 && e.line[i-1] == ' ' {
			i--
		}
		for i > 0 && e.line[i-1] != ' ' {
			i--
		}
		e.line = append(e.line[:i], e.line[e.cur:]...)
		e.cur = i
	case KeyCtrlL:
		return Event{Clear: true}
	case KeyCtrlC:
		e.Reset()
		return Event{Interrupt: true}
	case KeyUp, KeyCtrlP:
		if e.hist == 0 {
			return Event{Bell: true}
		}
		if e.hist == len(e.history) {
			e.saved = append([]rune(nil), e.line...)
		}
		e.hist--
		e.setLine(e.history[e.hist])
	case KeyDown, KeyCtrlN:
		if e.hist >= len(e.history) {
			return Event{Bell: true}
		}
		e.hist++
		if e.hist == len(e.history) {
			e.line = e.saved
			e.cur = len(e.line)
		} else {
			e.setLine(e.history[e.hist])
		}
	case KeyCtrlR:
		e.mode, e.query, e.match = modeSearch, nil, -1
	case KeyTab:
		return e.complete(ctx)
	}
	return Event{}
}

func (e *Editor) complete(ctx context.Context) Event {
	if e.opt.Complete == nil {
		return Event{Bell: true}
	}
	before := string(e.line[:e.cur])
	after := e.line[e.cur:]
	if e.lastTab && len(e.pending) > 1 {
		cands := e.pending
		e.lastTab = false
		if len(cands) > listThreshold {
			e.mode = modeAskAll
			return Event{Print: []string{askAll(len(cands))}}
		}
		e.pending = nil
		return Event{Print: Format(cands, e.opt.Cols)}
	}
	p := SplitPartial(before)
	raw, noSpace := e.opt.Complete(ctx, p.Words, p.Current)
	var cands []cli.Candidate
	for _, c := range raw {
		if cc, ok := sanitize(c); ok {
			cands = append(cands, cc)
		}
	}
	newBefore := before
	switch len(cands) {
	case 0:
		return Event{Bell: true}
	case 1:
		ins := Quote(cands[0].Value, p.Quote, true)
		if !noSpace {
			ins += " "
		}
		newBefore = before[:p.Start] + ins
	default:
		if lcp := commonPrefix(cands); len(lcp) > len(p.Current) {
			newBefore = before[:p.Start] + Quote(lcp, p.Quote, false)
		}
		e.lastTab, e.pending = true, cands
	}
	if len([]rune(newBefore))+len(after) > MaxLine {
		return Event{Bell: true}
	}
	e.line = append([]rune(newBefore), after...)
	e.cur = len([]rune(newBefore))
	if len(cands) > 1 && newBefore == before {
		return Event{Bell: true}
	}
	return Event{}
}

func (e *Editor) search(from int) {
	q := string(e.query)
	for i := from; i >= 0; i-- {
		if strings.Contains(e.history[i], q) {
			e.match = i
			return
		}
	}
}

func (e *Editor) searchKey(ctx context.Context, k Key) Event {
	switch k.Code {
	case KeyRune:
		e.query = append(e.query, k.Rune)
		start := len(e.history) - 1
		if e.match >= 0 {
			start = e.match
		}
		e.match = -1
		e.search(start)
	case KeyBackspace:
		if len(e.query) > 0 {
			e.query = e.query[:len(e.query)-1]
			e.match = -1
			e.search(len(e.history) - 1)
		}
	case KeyCtrlR:
		if e.match <= 0 {
			return Event{Bell: true}
		}
		m := e.match
		e.search(m - 1)
		if e.match == m {
			return Event{Bell: true}
		}
	case KeyCtrlG, KeyCtrlC, KeyEscape:
		e.mode = modeEdit
		return Event{}
	case KeyEnter:
		e.accept()
		return e.Key(ctx, k)
	default:
		e.accept()
		return e.Key(ctx, k)
	}
	return Event{}
}

func (e *Editor) accept() {
	e.mode = modeEdit
	if e.match >= 0 {
		e.setLine(e.history[e.match])
		e.hist = len(e.history)
	}
}
