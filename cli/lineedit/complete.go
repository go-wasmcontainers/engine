package lineedit

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/go-wasmcontainers/engine/cli"
)

// Completer lists the candidates for the word being typed: words are the
// complete words before it, current is its (unquoted) prefix. noSpace asks
// that a unique match get no trailing space.
type Completer func(ctx context.Context, words []string, current string) (cands []cli.Candidate, noSpace bool)

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

// sanitize keeps a candidate safe to show and to insert: a value with a
// control character is dropped (it could not be typed back), a description
// loses its control characters and escapes, and is bounded.
func sanitize(c cli.Candidate) (cli.Candidate, bool) {
	if c.Value == "" || !utf8.ValidString(c.Value) || strings.ContainsFunc(c.Value, isControl) {
		return c, false
	}
	d := strings.ToValidUTF8(c.Description, "�")
	d = strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return ' '
		}
		if isControl(r) {
			return -1
		}
		return r
	}, d)
	if r := []rune(d); len(r) > 200 {
		d = string(r[:200])
	}
	c.Description = d
	return c, true
}

func isControl(r rune) bool { return r < 0x20 || r == 0x7f || r >= 0x80 && r <= 0x9f }

func commonPrefix(cs []cli.Candidate) string {
	if len(cs) == 0 {
		return ""
	}
	p := cs[0].Value
	for _, c := range cs[1:] {
		for !strings.HasPrefix(c.Value, p) {
			_, size := utf8.DecodeLastRuneInString(p)
			p = p[:len(p)-size]
		}
	}
	return p
}

// Format lays candidates out for a terminal cols wide: one aligned
// "value  -- description" line each when any has a description (as zsh
// and fish show them), columns filled top to bottom otherwise (as bash).
func Format(cs []cli.Candidate, cols int) []string {
	if cols < 20 {
		cols = 80
	}
	w, desc := 0, false
	for _, c := range cs {
		w = max(w, utf8.RuneCountInString(c.Value))
		desc = desc || c.Description != ""
	}
	var out []string
	if desc {
		for _, c := range cs {
			line := c.Value
			if c.Description != "" {
				line += strings.Repeat(" ", w-utf8.RuneCountInString(c.Value)) + "  -- " + c.Description
			}
			if r := []rune(line); len(r) > cols-1 {
				line = string(r[:cols-2]) + "…"
			}
			out = append(out, line)
		}
		return out
	}
	colw := w + 2
	per := max(1, (cols-1)/colw)
	rows := (len(cs) + per - 1) / per
	for r := 0; r < rows; r++ {
		var b strings.Builder
		for c := 0; c < per; c++ {
			i := c*rows + r
			if i >= len(cs) {
				break
			}
			b.WriteString(cs[i].Value)
			if (c+1)*rows+r < len(cs) {
				b.WriteString(strings.Repeat(" ", colw-utf8.RuneCountInString(cs[i].Value)))
			}
		}
		out = append(out, b.String())
	}
	return out
}

// listThreshold is where a listing asks first, as readline does.
const listThreshold = 100

func askAll(n int) string { return fmt.Sprintf("Display all %d possibilities? (y or n)", n) }
