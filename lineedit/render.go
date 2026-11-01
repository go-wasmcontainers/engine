package lineedit

import (
	"fmt"
	"strings"
)

// RuneWidth is the number of terminal columns r takes: 0 for a combining
// mark or a zero-width character (a joiner, a variation selector), 2 for the wide and
// fullwidth East Asian characters and the emoji blocks, 1 otherwise. The
// wide ranges are an approximation of Unicode's East Asian Width property
// (UAX #11), enough to place the cursor for CJK text and emoji.
func RuneWidth(r rune) int {
	if r < 0x300 {
		return 1
	}
	for _, w := range zeroRanges {
		if r >= w[0] && r <= w[1] {
			return 0
		}
	}
	for _, w := range wideRanges {
		if r >= w[0] && r <= w[1] {
			return 2
		}
	}
	return 1
}

// zeroRanges are the main combining-mark blocks and the zero-width and
// variation-selector characters; the Unicode tables (unicode.Mn) would link
// tens of kilobytes into a WebAssembly guest for the same purpose.
var zeroRanges = [...][2]rune{
	{0x300, 0x36F}, {0x483, 0x489}, {0x591, 0x5BD}, {0x610, 0x61A}, {0x64B, 0x65F},
	{0x1AB0, 0x1AFF}, {0x1DC0, 0x1DFF}, {0x200B, 0x200F}, {0x2028, 0x202E},
	{0x2060, 0x2064}, {0x20D0, 0x20FF}, {0xFE00, 0xFE0F}, {0xFE20, 0xFE2F},
	{0xFEFF, 0xFEFF}, {0xE0100, 0xE01EF},
}

var wideRanges = [...][2]rune{
	{0x1100, 0x115F}, {0x2E80, 0x303E}, {0x3041, 0x33FF}, {0x3400, 0x4DBF},
	{0x4E00, 0xA4CF}, {0xAC00, 0xD7A3}, {0xF900, 0xFAFF}, {0xFE30, 0xFE6F},
	{0xFF00, 0xFF60}, {0xFFE0, 0xFFE6}, {0x1F300, 0x1F64F}, {0x1F900, 0x1F9FF},
	{0x20000, 0x3FFFD},
}

// Width is the number of terminal columns s takes (see RuneWidth).
func Width(s string) int {
	n := 0
	for _, r := range s {
		n += RuneWidth(r)
	}
	return n
}

// headRunes is the first n runes of s (all of s if it is shorter).
func headRunes(s string, n int) []rune {
	rs := []rune(s)
	return rs[:max(0, min(n, len(rs)))]
}

// Renderer turns States into ANSI output for a terminal that does not wrap
// lines on its own behalf (xterm.js does: the renderer accounts for it).
// It counts columns with RuneWidth; a prompt holding escape sequences is
// counted as the text it is, so a coloured prompt belongs in PromptStyle.
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
	p := Width(s.Prompt)
	total := p + Width(s.Line)
	endRow := total / cols
	if total > 0 && total%cols == 0 {
		// The cursor sits past the last column; force the wrap so the
		// arithmetic below holds.
		b.WriteString(" \r")
	}
	target := p + Width(string(headRunes(s.Line, s.Cursor)))
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
	total := Width(s.Prompt) + Width(s.Line)
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
