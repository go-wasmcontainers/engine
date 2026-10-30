package container

import (
	"bytes"
	"io"
	"sync"
	"sync/atomic"

	"github.com/go-wasmcontainers/engine/wasi"
)

// The terminal of an exec -t: the engine is the line discipline a kernel
// would be. Input arrives raw from the client (the client's terminal is
// in raw mode); in line mode it is edited and handed to the process a
// line at a time, echoed when echo is on; output newlines become CR LF.
// The guest changes echo and line mode with WASIX tty_set; there are no
// signals, so ^C erases the line being typed instead of interrupting.

// MaxLine bounds a line being edited.
const MaxLine = 4096

const (
	keyIntr  = 0x03 // ^C
	keyEOF   = 0x04 // ^D
	keyErase = 0x7f // DEL
	keyBS    = 0x08 // ^H
	keyKill  = 0x15 // ^U
)

// ldisc copies in to the process's stdin (pw), echoing to echo.
type ldisc struct {
	term *wasi.Terminal
	pw   *io.PipeWriter
	echo io.Writer

	mu   sync.Mutex // orders the pending line's hand-over with new keys
	line []byte
	// flushing bounds the hand-over to one goroutine at a time: a guest
	// that loops on tty_set without reading cannot grow host goroutines.
	flushing atomic.Bool
}

// newLdisc returns the line discipline of term, writing to pw. When the
// process turns line mode off (a line editor switching to raw mode), the
// line being typed is handed over at once, as a kernel does when ICANON
// is cleared: keys typed ahead are not lost in the switch.
func newLdisc(term *wasi.Terminal, pw *io.PipeWriter, echo io.Writer) *ldisc {
	l := &ldisc{term: term, pw: pw, echo: echo}
	term.OnMode(func(_, line bool) {
		// Not here: SetMode runs in the guest's tty_set, and the pipe
		// write waits for the guest to read. A switch made while a flush
		// runs needs no second one: the pending line is handed over by
		// that flush, or by the key path (key flushes first in raw mode).
		if !line && l.flushing.CompareAndSwap(false, true) {
			go func() {
				defer l.flushing.Store(false)
				l.flush()
			}()
		}
	})
	return l
}

// flush hands the pending line over if line mode is off.
func (l *ldisc) flush() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, line := l.term.Mode(); !line {
		l.flushLocked()
	}
}

func (l *ldisc) flushLocked() bool {
	if len(l.line) == 0 {
		return true
	}
	_, err := l.pw.Write(l.line)
	l.line = l.line[:0]
	return err == nil
}

// run copies until in ends or the process stops reading; it closes pw.
func (l *ldisc) run(in io.Reader) {
	defer l.pw.Close()
	buf := make([]byte, 512)
	for {
		n, err := in.Read(buf)
		for _, c := range buf[:n] {
			if !l.key(c) {
				return
			}
		}
		if err != nil {
			l.mu.Lock()
			l.flushLocked()
			l.mu.Unlock()
			return
		}
	}
}

// key handles one input byte; false once the process's stdin is closed.
func (l *ldisc) key(c byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	echo, line := l.term.Mode()
	if !line {
		if !l.flushLocked() { // typed in line mode, before the switch
			return false
		}
		if echo {
			l.echoBytes([]byte{c})
		}
		_, err := l.pw.Write([]byte{c})
		return err == nil
	}
	if c == '\r' {
		c = '\n' // ICRNL
	}
	switch c {
	case keyErase, keyBS:
		if len(l.line) > 0 {
			l.line = l.line[:len(l.line)-1]
			if echo {
				l.echoBytes([]byte("\b \b"))
			}
		}
		return true
	case keyKill:
		if echo {
			l.echoBytes(bytes.Repeat([]byte("\b \b"), len(l.line)))
		}
		l.line = l.line[:0]
		return true
	case keyIntr:
		if echo {
			l.echoBytes([]byte("^C\r\n"))
		}
		l.line = l.line[:0]
		return true
	case keyEOF:
		if len(l.line) == 0 {
			return false // end of file: run closes pw
		}
		_, err := l.pw.Write(l.line)
		l.line = l.line[:0]
		return err == nil
	}
	if len(l.line) >= MaxLine && c != '\n' {
		return true // a full line takes only its end
	}
	l.line = append(l.line, c)
	if echo {
		if c == '\n' {
			l.echoBytes([]byte("\r\n"))
		} else {
			l.echoBytes([]byte{c})
		}
	}
	if c == '\n' {
		_, err := l.pw.Write(l.line)
		l.line = l.line[:0]
		return err == nil
	}
	return true
}

func (l *ldisc) echoBytes(b []byte) {
	if l.echo != nil {
		l.echo.Write(b)
	}
}

// crlf turns "\n" into "\r\n" on a terminal's output (ONLCR); it
// serialises writes from the process and from the echo.
type crlf struct {
	mu sync.Mutex
	w  io.Writer
}

func (c *crlf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if bytes.IndexByte(p, '\n') < 0 {
		return c.w.Write(p)
	}
	out := make([]byte, 0, len(p)+8)
	for i, b := range p {
		if b == '\n' && (i == 0 || p[i-1] != '\r') {
			out = append(out, '\r')
		}
		out = append(out, b)
	}
	if _, err := c.w.Write(out); err != nil {
		return 0, err
	}
	return len(p), nil
}

// rawEcho writes echo bytes as they are (the ldisc already writes CR LF).
type rawEcho struct{ c *crlf }

func (r rawEcho) Write(p []byte) (int, error) {
	r.c.mu.Lock()
	defer r.c.mu.Unlock()
	return r.c.w.Write(p)
}
