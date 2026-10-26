// Package inproc carries HTTP requests to a handler in the same process,
// with no socket: the browser has no unix sockets, so the playground's
// wctr talks to its engine through this transport instead.
//
// Responses stream: RoundTrip returns as soon as the handler has written
// its header (explicitly, by flushing, or with the first body byte), and
// the body is read while the handler still runs — what the container
// wait and log endpoints rely on.
package inproc

import (
	"context"
	"io"
	"net/http"
	"sync"
)

// Transport serves every request with Handler.
type Transport struct {
	Handler http.Handler
}

// RoundTrip implements http.RoundTripper.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx, cancel := context.WithCancel(req.Context())
	sreq := req.Clone(ctx)
	if sreq.Body == nil {
		sreq.Body = http.NoBody
	}
	sreq.RequestURI = req.URL.RequestURI()
	sreq.RemoteAddr = "inproc"
	pr, pw := io.Pipe()
	w := &writer{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	go func() {
		defer func() {
			w.writeHeader(http.StatusOK) // a handler that wrote nothing
			pw.Close()
		}()
		t.Handler.ServeHTTP(w, sreq)
	}()
	select {
	case <-w.ready:
	case <-ctx.Done():
		cancel()
		pr.CloseWithError(ctx.Err())
		return nil, ctx.Err()
	}
	return &http.Response{
		Status:     http.StatusText(w.status),
		StatusCode: w.status,
		Proto:      "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header:        w.sent,
		Body:          &body{pr: pr, cancel: cancel},
		ContentLength: -1,
		Request:       req,
	}, nil
}

type body struct {
	pr     *io.PipeReader
	cancel context.CancelFunc
	once   sync.Once
}

func (b *body) Read(p []byte) (int, error) { return b.pr.Read(p) }

// Close ends the request: the handler's context is cancelled, as when a
// network client hangs up.
func (b *body) Close() error {
	b.once.Do(func() {
		b.cancel()
		b.pr.CloseWithError(io.ErrClosedPipe)
	})
	return nil
}

type writer struct {
	header http.Header
	sent   http.Header
	pw     *io.PipeWriter
	status int
	once   sync.Once
	ready  chan struct{}
}

func (w *writer) Header() http.Header { return w.header }

func (w *writer) writeHeader(code int) {
	w.once.Do(func() {
		w.status = code
		w.sent = w.header.Clone()
		close(w.ready)
	})
}

func (w *writer) WriteHeader(code int) { w.writeHeader(code) }

func (w *writer) Write(p []byte) (int, error) {
	w.writeHeader(http.StatusOK)
	return w.pw.Write(p)
}

// Flush implements http.Flusher: the header goes out; the pipe has no
// buffer, so bytes already are where the reader can see them.
func (w *writer) Flush() { w.writeHeader(http.StatusOK) }
