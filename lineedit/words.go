package lineedit

// Split splits a terminal line into words the way a POSIX shell would for
// simple commands: blanks separate words, single quotes keep everything
// literal, double quotes keep blanks (with \" \\ \$ \` escapes), a
// backslash outside quotes escapes the next character. No expansion, no
// pipes, no redirections: one wctr command per line.

import (
	"errors"
	"strings"
)

// ErrUnterminated is returned for an open quote or a trailing backslash.
var ErrUnterminated = errors.New("unterminated quote or escape")

// MaxLine bounds the line the splitter accepts.
const MaxLine = 4096

// Split returns the words of line.
func Split(line string) ([]string, error) {
	if len(line) > MaxLine {
		return nil, errors.New("line too long")
	}
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case c == ' ' || c == '\t':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			inWord = true
			j := strings.IndexByte(line[i+1:], '\'')
			if j < 0 {
				return nil, ErrUnterminated
			}
			cur.WriteString(line[i+1 : i+1+j])
			i += j + 1
		case c == '"':
			inWord = true
			i++
			for ; ; i++ {
				if i >= len(line) {
					return nil, ErrUnterminated
				}
				d := line[i]
				if d == '"' {
					break
				}
				if d == '\\' && i+1 < len(line) && strings.IndexByte("\"\\$`", line[i+1]) >= 0 {
					i++
					d = line[i]
				}
				cur.WriteByte(d)
			}
		case c == '\\':
			if i+1 >= len(line) {
				return nil, ErrUnterminated
			}
			inWord = true
			i++
			cur.WriteByte(line[i])
		default:
			inWord = true
			cur.WriteByte(c)
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words, nil
}
