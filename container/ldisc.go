package container

import (
	"bytes"
	"io"
	"sync"

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
	line []byte
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
			if len(l.line) > 0 {
				l.pw.Write(l.line)
			}
			return
		}
	}
}

// key handles one input byte; false once the process's stdin is closed.
func (l *ldisc) key(c byte) bool {
	echo, line := l.term.Mode()
	if !line {
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
