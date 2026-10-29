package container

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/go-wasmcontainers/engine/wasi"
)

// A container's own stdin and terminal (run -i, run -t; Docker's
// Config.OpenStdin, StdinOnce and Tty). With OpenStdin the first process
// reads a pipe that attached clients write into; with Tty it has a
// terminal, whose line discipline is exec -t's (ldisc.go), and its output,
// CR LF and echo included, goes to the logs as stdout. Each start makes
// them anew.

// Errors of attach and resize.
var (
	ErrNoStdin    = errors.New("container has no stdin (create it with OpenStdin, run -i)")
	ErrNoTerminal = errors.New("container has no terminal (create it with Tty, run -t)")
	// ErrDetached ends an attach whose input carried the detach keys.
	ErrDetached = errors.New("detached")
)

// DefaultDetachKeys are Docker's: Ctrl-P Ctrl-Q.
const DefaultDetachKeys = "ctrl-p,ctrl-q"

// stdinHub is one run's stdin: what attached clients send goes to the
// process, through the terminal's line discipline when there is one.
type stdinHub struct {
	mu     sync.Mutex // serialises clients: one write at a time
	pw     *io.PipeWriter
	ld     *ldisc // nil without a terminal
	closed bool
	done   chan struct{}
}

func newStdinHub(pw *io.PipeWriter, ld *ldisc) *stdinHub {
	return &stdinHub{pw: pw, ld: ld, done: make(chan struct{})}
}

// write hands p to the process; an error once its stdin is closed.
func (h *stdinHub) write(p []byte) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return io.ErrClosedPipe
	}
	if h.ld == nil {
		_, err := h.pw.Write(p)
		return err
	}
	for _, c := range p {
		if !h.ld.key(c) {
			h.closeLocked() // ^D on an empty line: end of file
			return io.ErrClosedPipe
		}
	}
	return nil
}

// close ends the process's stdin (a pending line is delivered first).
func (h *stdinHub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closeLocked()
}

func (h *stdinHub) closeLocked() {
	if h.closed {
		return
	}
	h.closed = true
	if h.ld != nil && len(h.ld.line) > 0 {
		h.pw.Write(h.ld.line)
	}
	h.pw.Close()
	close(h.done)
}

// stopped closes the hub when the process ends, without writing: nobody
// reads any more, and a write would block.
func (h *stdinHub) stopped() {
	h.pw.CloseWithError(io.ErrClosedPipe) // unblocks a writer first
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.closed {
		h.closed = true
		close(h.done)
	}
}

// ParseDetachKeys reads a detach sequence as Docker does (moby/term
// ToBytes): comma-separated keys, each one character or ctrl-@, ctrl-a …
// ctrl-z, ctrl-[, ctrl-\, ctrl-], ctrl-^, ctrl-_. "" is the default; at
// most 16 keys.
func ParseDetachKeys(s string) ([]byte, error) {
	if s == "" {
		s = DefaultDetachKeys
	}
	var out []byte
	for _, k := range strings.Split(s, ",") {
		k = strings.TrimSpace(k)
		switch {
		case len(k) == 1:
			out = append(out, k[0])
		case len(k) == 6 && strings.HasPrefix(strings.ToLower(k), "ctrl-"):
			c := k[5]
			switch {
			case c >= 'a' && c <= 'z':
				out = append(out, c-'a'+1)
			case c >= 'A' && c <= 'Z':
				out = append(out, c-'A'+1)
			case c == '@':
				out = append(out, 0)
			case c >= '[' && c <= '_': // [ \ ] ^ _
				out = append(out, c-'['+27)
			default:
				return nil, fmt.Errorf("%w: unknown detach key %q", ErrInvalid, k)
			}
		case k == "DEL":
			out = append(out, 127)
		default:
			return nil, fmt.Errorf("%w: unknown detach key %q", ErrInvalid, k)
		}
		if len(out) > 16 {
			return nil, fmt.Errorf("%w: detach sequence longer than 16 keys", ErrInvalid)
		}
	}
	return out, nil
}

// detachScanner passes input through, holding back a partial match of
// the detach sequence; the full sequence is never passed on.
type detachScanner struct {
	keys    []byte
	matched int
}

// scan returns what to pass on from p, and whether the sequence completed
// (what came before it in p is passed on; what follows, dropped).
func (d *detachScanner) scan(p []byte) ([]byte, bool) {
	if len(d.keys) == 0 {
		return p, false
	}
	out := make([]byte, 0, len(p)+len(d.keys))
	for _, c := range p {
		if c == d.keys[d.matched] {
			d.matched++
			if d.matched == len(d.keys) {
				return out, true
			}
			continue
		}
		// A mismatch: the held prefix was input after all.
		out = append(out, d.keys[:d.matched]...)
		d.matched = 0
		if c == d.keys[0] {
			d.matched = 1
			if len(d.keys) == 1 {
				return out, true
			}
			continue
		}
		out = append(out, c)
	}
	return out, false
}

// AttachStdin copies in to the container's running process until in ends,
// the process ends, ctx is done, or in carries the detach keys (then it
// returns ErrDetached). With StdinOnce, the end of in closes the
// process's stdin, as the first attached client's hang-up does in Docker.
func (c *Container) AttachStdin(ctx context.Context, in io.Reader, detachKeys []byte) error {
	if !c.cfg.OpenStdin {
		return fmt.Errorf("%w: %s", ErrNoStdin, c.id[:12])
	}
	// Attached before the start (run attaches first, so nothing typed is
	// lost): wait for the run's stdin.
	var h *stdinHub
	for {
		c.mu.Lock()
		hub, running, removed, changed := c.stdin, c.state == Running, c.removed, c.changed
		c.mu.Unlock()
		if removed {
			return nil
		}
		if hub != nil && running {
			select {
			case <-hub.done:
			default:
				h = hub
			}
		}
		if h != nil {
			break
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	once := c.cfg.StdinOnce
	d := detachScanner{keys: detachKeys}
	type chunk struct {
		b   []byte
		err error
	}
	reads := make(chan chunk)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			select {
			case reads <- chunk{append([]byte(nil), buf[:n]...), err}:
			case <-stop:
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-h.done:
			return nil
		case r := <-reads:
			out, detached := d.scan(r.b)
			if len(out) > 0 {
				if err := h.write(out); err != nil {
					return nil // the process closed its stdin, or ended
				}
			}
			if detached {
				return ErrDetached
			}
			if r.err != nil {
				if once {
					h.close()
				}
				return nil
			}
		}
	}
}

// Resize sets the size of the container's terminal (run -t) while it runs.
func (c *Container) Resize(h, w uint16) error {
	c.mu.Lock()
	t, running := c.term, c.state == Running
	c.mu.Unlock()
	if t == nil || !running {
		return fmt.Errorf("%w: %s", ErrNoTerminal, c.id[:12])
	}
	if h == 0 || w == 0 {
		return fmt.Errorf("%w: size %dx%d", ErrInvalid, w, h)
	}
	t.SetSize(w, h)
	return nil
}

// stdio sets up a run's stdin and terminal (called with c.mu held); out
// is the logs' stdout. It returns what the spec gets, and the function
// that ends them when the process ends.
func (c *Container) stdioLocked(out io.Writer) (stdin io.Reader, stdout, stderr io.Writer, term *wasi.Terminal, end func()) {
	cfg := c.cfg
	stdout, stderr = nil, nil
	var cr *crlf
	if cfg.Tty {
		h, w := cfg.ConsoleSize[0], cfg.ConsoleSize[1]
		if h == 0 || w == 0 {
			h, w = 24, 80
		}
		term = wasi.NewTerminal(w, h, [3]bool{cfg.OpenStdin, true, true})
		cr = &crlf{w: out}
		stdout, stderr = cr, cr
	}
	var hub *stdinHub
	if cfg.OpenStdin {
		pr, pw := io.Pipe()
		var ld *ldisc
		if term != nil {
			ld = &ldisc{term: term, pw: pw, echo: rawEcho{cr}}
		}
		hub = newStdinHub(pw, ld)
		stdin = pr
	}
	c.stdin, c.term = hub, term
	return stdin, stdout, stderr, term, func() {
		if hub != nil {
			hub.stopped()
		}
	}
}
