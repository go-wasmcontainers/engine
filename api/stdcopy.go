package api

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

// Stream types of the multiplexed stream.
const (
	StreamStdin  byte = 0
	StreamStdout byte = 1
	StreamStderr byte = 2
)

// maxFrame bounds one frame the demultiplexer accepts, so a hostile or
// broken server cannot make the client allocate without bound.
const maxFrame = 1 << 24

// WriteFrame writes one frame: header [8]byte{STREAM_TYPE, 0, 0, 0,
// SIZE1..SIZE4 big-endian}, then the payload.
func WriteFrame(w io.Writer, stream byte, p []byte) error {
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxFrame {
			chunk = chunk[:maxFrame]
		}
		var h [8]byte
		h[0] = stream
		binary.BigEndian.PutUint32(h[4:], uint32(len(chunk)))
		if _, err := w.Write(h[:]); err != nil {
			return err
		}
		if _, err := w.Write(chunk); err != nil {
			return err
		}
		p = p[len(chunk):]
	}
	return nil
}

// ErrBadFrame is returned for a malformed multiplexed stream.
var ErrBadFrame = errors.New("malformed multiplexed stream")

// Demux copies a multiplexed stream to stdout and stderr (stdin frames,
// type 0, go to stdout as the format says), until EOF.
func Demux(stdout, stderr io.Writer, r io.Reader) error {
	var h [8]byte
	buf := make([]byte, 32<<10)
	for {
		if _, err := io.ReadFull(r, h[:]); err != nil {
			if err == io.EOF {
				return nil
			}
			return fmt.Errorf("%w: %v", ErrBadFrame, err)
		}
		var w io.Writer
		switch h[0] {
		case StreamStdin, StreamStdout:
			w = stdout
		case StreamStderr:
			w = stderr
		default:
			return fmt.Errorf("%w: stream type %d", ErrBadFrame, h[0])
		}
		n := binary.BigEndian.Uint32(h[4:])
		if n > maxFrame {
			return fmt.Errorf("%w: frame of %d bytes", ErrBadFrame, n)
		}
		for n > 0 {
			chunk := buf
			if uint32(len(chunk)) > n {
				chunk = chunk[:n]
			}
			if _, err := io.ReadFull(r, chunk); err != nil {
				return fmt.Errorf("%w: %v", ErrBadFrame, err)
			}
			if _, err := w.Write(chunk); err != nil {
				return err
			}
			n -= uint32(len(chunk))
		}
	}
}
