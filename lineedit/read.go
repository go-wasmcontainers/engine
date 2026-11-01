package lineedit

import (
	"context"
	"errors"
	"io"
	"strings"
	"unicode/utf8"
)

// ErrInterrupt is what Reader.ReadLine returns for Ctrl-C.
var ErrInterrupt = errors.New("interrupt")

// maxSeq bounds the bytes of an escape sequence that are kept; a longer
// one is consumed and ignored.
const maxSeq = 32

// Reader edits lines read from a terminal in raw mode (no echo, no line
// buffering): it decodes the bytes into keys, applies them to an Editor
// and draws the result with a Renderer. It reads one byte at a time, so
// nothing past the line is consumed from In. Putting the terminal in raw
// mode is the caller's business (and only while a line is being typed).
type Reader struct {
	In  io.Reader
	Out io.Writer
	// Cols returns the terminal width (nil, or 0 or less: 80).
	Cols func() int
	// Complete answers Tab; nil disables completion.
	Complete Completer
	// HistorySize bounds the history (0: 500).
	HistorySize int
	// PromptStyle wraps the prompt as Renderer.PromptStyle does; "" is
	// none, which is what a shell wants (its prompt carries its own style).
	PromptStyle string

	ed *Editor
	b  [1]byte
}

// Editor returns the Reader's editor, to read or restore its history.
func (r *Reader) Editor() *Editor {
	if r.ed == nil {
		r.ed = New(Options{Complete: r.Complete, HistorySize: r.HistorySize})
	}
	return r.ed
}

func (r *Reader) cols() int {
	if r.Cols != nil {
		return r.Cols()
	}
	return 0
}

func (r *Reader) readByte() (byte, error) {
	for {
		n, err := r.In.Read(r.b[:])
		if n == 1 {
			return r.b[0], nil
		}
		if err != nil {
			return 0, err
		}
	}
}

// readUnit reads one typed unit: a character, a control byte or a whole
// escape sequence, as the string ParseKeys understands. An ESC followed by
// a byte that opens no sequence (an Alt-key) gives "", which is ignored.
func (r *Reader) readUnit() (string, error) {
	c, err := r.readByte()
	if err != nil {
		return "", err
	}
	switch {
	case c == 0x1b:
		return r.readEscape()
	case c < utf8.RuneSelf:
		return string(rune(c)), nil
	}
	n := 0
	switch {
	case c&0xe0 == 0xc0:
		n = 1
	case c&0xf0 == 0xe0:
		n = 2
	case c&0xf8 == 0xf0:
		n = 3
	default:
		return "\x80", nil // a stray byte: an unknown key
	}
	b := []byte{c}
	for range n {
		d, err := r.readByte()
		if err != nil {
			return "", err
		}
		b = append(b, d)
	}
	return string(b), nil
}

func (r *Reader) readEscape() (string, error) {
	c, err := r.readByte()
	if err != nil {
		return "", err
	}
	switch c {
	case 'O': // SS3: one final byte
		f, err := r.readByte()
		if err != nil {
			return "", err
		}
		return "\x1bO" + string(rune(f)), nil
	case '[': // CSI: parameter and intermediate bytes, then a final byte
		seq := []byte{0x1b, '['}
		for {
			f, err := r.readByte()
			if err != nil {
				return "", err
			}
			if f >= 0x40 && f <= 0x7e {
				if len(seq) > maxSeq {
					return "\x1b[Z", nil // too long to be a key we know
				}
				return string(append(seq, f)), nil
			}
			if f < 0x20 || f > 0x7e {
				return "", nil // not a CSI sequence after all: dropped
			}
			if len(seq) <= maxSeq {
				seq = append(seq, f)
			}
		}
	}
	return "", nil
}

// ReadLine shows prompt and returns the line typed, without its newline.
// It returns io.EOF for Ctrl-D on an empty line (or the end of In with
// nothing typed), ErrInterrupt for Ctrl-C, and the error of In otherwise.
// A prompt of several lines is written once, its last line redrawn.
func (r *Reader) ReadLine(ctx context.Context, prompt string) (string, error) {
	head, tail := "", prompt
	if i := strings.LastIndexByte(prompt, '\n'); i >= 0 {
		head, tail = prompt[:i+1], prompt[i+1:]
	}
	ed := r.Editor()
	ed.Reset()
	ed.SetPrompt(tail)
	ed.SetCols(r.cols())
	style := r.PromptStyle
	if style == "" {
		style = "-"
	}
	rd := &Renderer{Cols: r.cols(), PromptStyle: style}
	io.WriteString(r.Out, head+rd.Render(ed.State()))
	for {
		unit, err := r.readUnit()
		if err != nil {
			st := ed.State()
			line := st.Line
			if line != "" && errors.Is(err, io.EOF) {
				io.WriteString(r.Out, rd.Leave(st))
				ed.addHistory(line)
				return line, nil
			}
			io.WriteString(r.Out, rd.Leave(st))
			return "", err
		}
		for _, k := range ParseKeys(unit) {
			st := ed.State()
			ev := ed.Key(ctx, k)
			switch {
			case ev.Submit:
				// The line may have come from a search: show it as it is.
				end := State{Prompt: tail, Line: ev.Line, Cursor: utf8.RuneCountInString(ev.Line)}
				io.WriteString(r.Out, rd.Render(end)+rd.Leave(end))
				return ev.Line, nil
			case ev.Interrupt:
				st.Line += "^C"
				st.Cursor = utf8.RuneCountInString(st.Line)
				io.WriteString(r.Out, rd.Render(st)+rd.Leave(st))
				return "", ErrInterrupt
			case ev.EOF:
				io.WriteString(r.Out, rd.Leave(st))
				return "", io.EOF
			case ev.Clear:
				rd.Forget()
				io.WriteString(r.Out, "\x1b[H\x1b[2J"+rd.Render(ed.State()))
			case ev.Print != nil:
				io.WriteString(r.Out, rd.Print(ev.Print, ed.State()))
			case ev.Bell:
				io.WriteString(r.Out, "\a")
			default:
				io.WriteString(r.Out, rd.Render(ed.State()))
			}
		}
	}
}
