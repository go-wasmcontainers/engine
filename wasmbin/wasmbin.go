// Package wasmbin rewrites the one part of a WebAssembly binary the engine
// needs to change: the limits of the module's own linear memory.
//
// A browser cannot cap a memory the module defines itself, as wazero's
// WithMemoryLimitPages does; the cap has to be in the binary. Making the
// memory shared at the same time lets the engine's worker read and write
// the guest's memory directly while it serves WASI calls (the threads
// proposal's limits encoding: 0x01 min max, 0x03 min max shared).
package wasmbin

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrInvalid is wrapped by every refusal of a binary.
var ErrInvalid = errors.New("invalid WebAssembly binary")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

var header = []byte{0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00}

const (
	sectionImport = 2
	sectionMemory = 5
)

// MaxPages is the most pages a 32-bit memory can have.
const MaxPages = 65536

func readULEB(b []byte) (uint64, int, error) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		v |= uint64(b[i]&0x7f) << (7 * uint(i))
		if b[i]&0x80 == 0 {
			return v, i + 1, nil
		}
	}
	return 0, 0, invalid("bad LEB128")
}

func appendULEB(b []byte, v uint64) []byte {
	for {
		c := byte(v & 0x7f)
		v >>= 7
		if v != 0 {
			b = append(b, c|0x80)
			continue
		}
		return append(b, c)
	}
}

// LimitMemory returns a copy of mod whose defined memory has maximum
// maxPages (or its own maximum if lower), shared if asked. It refuses a
// module that imports its memory, defines more than one, or needs more
// than maxPages to start.
func LimitMemory(mod []byte, maxPages uint32, shared bool) ([]byte, error) {
	if maxPages == 0 || maxPages > MaxPages {
		return nil, fmt.Errorf("memory limit of %d pages is outside 1..%d", maxPages, MaxPages)
	}
	if !bytes.HasPrefix(mod, header) {
		return nil, invalid("no wasm header (or not version 1)")
	}
	out := append([]byte(nil), header...)
	rest := mod[len(header):]
	found := false
	for len(rest) > 0 {
		id := rest[0]
		size, n, err := readULEB(rest[1:])
		if err != nil {
			return nil, err
		}
		start := 1 + n
		if size > uint64(len(rest)-start) {
			return nil, invalid("section %d overruns the binary", id)
		}
		payload := rest[start : start+int(size)]
		switch id {
		case sectionImport:
			if err := checkImports(payload); err != nil {
				return nil, err
			}
			out = append(out, rest[:start+int(size)]...)
		case sectionMemory:
			if found {
				return nil, invalid("two memory sections")
			}
			found = true
			np, err := limitMemorySection(payload, maxPages, shared)
			if err != nil {
				return nil, err
			}
			out = append(out, sectionMemory)
			out = appendULEB(out, uint64(len(np)))
			out = append(out, np...)
		default:
			out = append(out, rest[:start+int(size)]...)
		}
		rest = rest[start+int(size):]
	}
	if !found {
		return nil, invalid("the module defines no memory")
	}
	return out, nil
}

func limitMemorySection(p []byte, maxPages uint32, shared bool) ([]byte, error) {
	count, n, err := readULEB(p)
	if err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, invalid("%d memories, want 1", count)
	}
	p = p[n:]
	if len(p) < 1 {
		return nil, invalid("truncated memory limits")
	}
	flags := p[0]
	if flags > 0x03 {
		return nil, invalid("unsupported memory limits flags %#x (memory64?)", flags)
	}
	min, n, err := readULEB(p[1:])
	if err != nil {
		return nil, err
	}
	used := 1 + n
	max := uint64(maxPages)
	if flags&0x01 != 0 {
		m, n2, err := readULEB(p[used:])
		if err != nil {
			return nil, err
		}
		used += n2
		if m < max {
			max = m
		}
	}
	if used != len(p) {
		return nil, invalid("trailing bytes in the memory section")
	}
	if min > MaxPages {
		return nil, invalid("memory minimum %d pages", min)
	}
	if min > max {
		return nil, fmt.Errorf("the module needs %d pages of memory to start, the limit is %d", min, max)
	}
	flag := byte(0x01)
	if shared || flags&0x02 != 0 {
		flag = 0x03
	}
	out := appendULEB(nil, 1)
	out = append(out, flag)
	out = appendULEB(out, min)
	out = appendULEB(out, max)
	return out, nil
}

// checkImports refuses an imported memory: its limits are the host's.
func checkImports(p []byte) error {
	count, n, err := readULEB(p)
	if err != nil {
		return err
	}
	p = p[n:]
	for i := uint64(0); i < count; i++ {
		for k := 0; k < 2; k++ { // module name, field name
			l, n, err := readULEB(p)
			if err != nil {
				return err
			}
			if l > uint64(len(p)-n) {
				return invalid("import name overruns")
			}
			p = p[n+int(l):]
		}
		if len(p) < 1 {
			return invalid("truncated import")
		}
		kind := p[0]
		p = p[1:]
		switch kind {
		case 0x00: // func: type index
			_, n, err := readULEB(p)
			if err != nil {
				return err
			}
			p = p[n:]
		case 0x01: // table: reftype + limits
			if len(p) < 2 {
				return invalid("truncated table import")
			}
			fl := p[1]
			p = p[2:]
			for j := 0; j < 1+int(fl&1); j++ {
				_, n, err := readULEB(p)
				if err != nil {
					return err
				}
				p = p[n:]
			}
		case 0x02:
			return invalid("the module imports its memory")
		case 0x03: // global: valtype + mut
			if len(p) < 2 {
				return invalid("truncated global import")
			}
			p = p[2:]
		case 0x04: // tag
			if len(p) < 1 {
				return invalid("truncated tag import")
			}
			_, n, err := readULEB(p[1:])
			if err != nil {
				return err
			}
			p = p[1+n:]
		default:
			return invalid("unknown import kind %#x", kind)
		}
	}
	return nil
}
