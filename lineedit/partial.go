package lineedit

import "strings"

// Partial is the line before the cursor, split for completion.
type Partial struct {
	// Words are the complete words before the one being typed.
	Words []string
	// Current is the word being typed, unquoted ("" for a new word).
	Current string
	// Start is the byte offset in the line where the current word begins.
	Start int
	// Quote is the quote the current word is still inside (' or "), or 0.
	Quote byte
}

// SplitPartial splits a line prefix the way Split does, except that the
// last word may be unterminated (an open quote, a trailing backslash).
func SplitPartial(prefix string) Partial {
	var p Partial
	var cur strings.Builder
	inWord := false
	start := 0
	var quote byte
	for i := 0; i < len(prefix); i++ {
		c := prefix[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case quote == '"':
			switch {
			case c == '"':
				quote = 0
			case c == '\\' && i+1 < len(prefix) && strings.IndexByte("\"\\$`", prefix[i+1]) >= 0:
				i++
				cur.WriteByte(prefix[i])
			default:
				cur.WriteByte(c)
			}
		case c == ' ' || c == '\t':
			if inWord {
				p.Words = append(p.Words, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			if !inWord {
				inWord = true
				start = i
			}
			switch c {
			case '\'', '"':
				quote = c
			case '\\':
				if i+1 < len(prefix) {
					i++
					cur.WriteByte(prefix[i])
				}
			default:
				cur.WriteByte(c)
			}
		}
	}
	if inWord {
		p.Current = cur.String()
		p.Start = start
		p.Quote = quote
	} else {
		p.Start = len(prefix)
	}
	return p
}

// special are the characters a completed word must escape outside quotes.
const special = " \t'\"\\$`&|;<>()*?[]#~!{}"

// Quote renders a word for insertion: inside the given open quote (0 for
// none), so that Split reads it back as w; closed adds the closing quote.
func Quote(w string, quote byte, closed bool) string {
	var b strings.Builder
	switch quote {
	case '\'':
		b.WriteByte('\'')
		for i := 0; i < len(w); i++ { // bytes, not runes: invalid UTF-8 kept as is
			if w[i] == '\'' {
				b.WriteString(`'\''`)
			} else {
				b.WriteByte(w[i])
			}
		}
		if closed {
			b.WriteByte('\'')
		}
	case '"':
		b.WriteByte('"')
		for i := 0; i < len(w); i++ {
			if strings.IndexByte("\"\\$`", w[i]) >= 0 {
				b.WriteByte('\\')
			}
			b.WriteByte(w[i])
		}
		if closed {
			b.WriteByte('"')
		}
	default:
		for i := 0; i < len(w); i++ {
			if strings.IndexByte(special, w[i]) >= 0 {
				b.WriteByte('\\')
			}
			b.WriteByte(w[i])
		}
	}
	return b.String()
}
