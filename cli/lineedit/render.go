package lineedit

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// Renderer turns States into ANSI output for a terminal that does not wrap
// lines on its own behalf (xterm.js does: the renderer accounts for it).
// It assumes one column per rune.
type Renderer struct {
	// Cols is the terminal width.
	Cols int
	// PromptStyle wraps the prompt (default bold cyan); "" for none.
	PromptStyle string

	shown  bool
	curRow int // the cursor's row within the rendered prompt+line
}

func (r *Renderer) cols() int {
	if r.Cols < 2 {
		return 80
	}
	return r.Cols
}

func (r *Renderer) style() (string, string) {
	if r.PromptStyle == "-" {
		return "", ""
	}
	if r.PromptStyle != "" {
		return r.PromptStyle, "\x1b[0m"
	}
	return "\x1b[1;36m", "\x1b[0m"
}

// Render returns the output that replaces what was last rendered with s.
func (r *Renderer) Render(s State) string {
	var b strings.Builder
	if r.shown {
		b.WriteByte('\r')
		if r.curRow > 0 {
			fmt.Fprintf(&b, "\x1b[%dA", r.curRow)
		}
		b.WriteString("\x1b[J")
	}
	on, off := r.style()
	b.WriteString(on + s.Prompt + off + s.Line)
	cols := r.cols()
	p := utf8.RuneCountInString(s.Prompt)
	total := p + utf8.RuneCountInString(s.Line)
	endRow := total / cols
	if total > 0 && total%cols == 0 {
		// The cursor sits past the last column; force the wrap so the
		// arithmetic below holds.
		b.WriteString(" \r")
	}
	target := p + s.Cursor
	tRow, tCol := target/cols, target%cols
	if up := endRow - tRow; up > 0 {
		fmt.Fprintf(&b, "\x1b[%dA", up)
	}
	b.WriteByte('\r')
	if tCol > 0 {
		fmt.Fprintf(&b, "\x1b[%dC", tCol)
	}
	r.shown, r.curRow = true, tRow
	return b.String()
}

// Print returns the output that shows lines above the input, then the
// input again below them.
func (r *Renderer) Print(lines []string, s State) string {
	var b strings.Builder
	b.WriteString(r.Leave(s))
	for _, l := range lines {
		b.WriteString(l + "\r\n")
	}
	b.WriteString(r.Render(s))
	return b.String()
}

// Leave moves the cursor below the input (after Enter, Ctrl-C) and forgets
// it: the next Render starts a fresh prompt.
func (r *Renderer) Leave(s State) string {
	if !r.shown {
		return ""
	}
	cols := r.cols()
	total := utf8.RuneCountInString(s.Prompt) + utf8.RuneCountInString(s.Line)
	endRow := total / cols
	var b strings.Builder
	if down := endRow - r.curRow; down > 0 {
		fmt.Fprintf(&b, "\x1b[%dB", down)
	}
	if total > 0 && total%cols == 0 {
		b.WriteString("\r\x1b[K") // already on a fresh row (the forced wrap)
	} else {
		b.WriteString("\r\n")
	}
	r.shown, r.curRow = false, 0
	return b.String()
}

// Forget drops what was rendered (the screen was cleared).
func (r *Renderer) Forget() { r.shown, r.curRow = false, 0 }
