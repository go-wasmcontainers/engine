package wazero

import (
	"sync/atomic"

	"github.com/tetratelabs/wazero/experimental"
)

// measuredMemory backs a guest's linear memory and publishes its size: a
// non-shared wazero memory is regrown without synchronisation, so reading
// its length from another goroutine would race; Reallocate runs on the
// guest's own goroutine when it grows, which is where the size is known.
type measuredMemory struct {
	buf  []byte
	max  uint64
	size *atomic.Int64
}

func (m *measuredMemory) Reallocate(size uint64) []byte {
	if size > m.max {
		return nil
	}
	if size <= uint64(cap(m.buf)) {
		m.buf = m.buf[:size] // a memory never shrinks: the bytes past the old length are still zero
	} else {
		c := max(size, min(2*uint64(cap(m.buf)), m.max))
		nb := make([]byte, size, c)
		copy(nb, m.buf)
		m.buf = nb
	}
	m.size.Store(int64(size))
	return m.buf
}

func (m *measuredMemory) Free() { m.buf = nil }

// allocator measures the memory of one process into size.
func allocator(size *atomic.Int64) experimental.MemoryAllocator {
	return experimental.MemoryAllocatorFunc(func(c, max uint64) experimental.LinearMemory {
		return &measuredMemory{buf: make([]byte, 0, c), max: max, size: size}
	})
}
