// Package virtio implements virtio 1.x devices over the MMIO transport.
//
// Security: everything in a virtqueue (ring indices, descriptor addresses,
// lengths, flags, chain links) is written by the untrusted guest. All guest
// memory access goes through Memory.Slice, which bounds-checks, and every
// ring index and chain is validated before use. A malformed queue makes the
// device stop processing; it never makes the VMM touch memory outside the
// guest's RAM.
package virtio

import (
	"errors"
	"fmt"
	"unsafe"
)

// Memory is the guest's physical RAM as seen by the host. Guest RAM is one
// contiguous region starting at guest physical address 0.
type Memory struct {
	b []byte
}

// NewMemory wraps the host mapping of guest RAM.
func NewMemory(b []byte) *Memory { return &Memory{b: b} }

// ErrOutOfBounds is returned for any guest address range outside RAM.
var ErrOutOfBounds = errors.New("virtio: guest address out of bounds")

// Slice returns the host view of guest physical range [gpa, gpa+n). It is
// the only way devices touch guest memory.
func (m *Memory) Slice(gpa, n uint64) ([]byte, error) {
	size := uint64(len(m.b))
	if gpa > size || n > size-gpa {
		return nil, fmt.Errorf("%w: [%#x, +%#x) with %#x bytes of RAM", ErrOutOfBounds, gpa, n, size)
	}
	return m.b[gpa : gpa+n : gpa+n], nil
}

// HostAddr returns the host virtual address backing guest range
// [gpa, gpa+n), for handing to the kernel (vhost). The range is checked.
func (m *Memory) HostAddr(gpa, n uint64) (uintptr, error) {
	s, err := m.Slice(gpa, n)
	if err != nil {
		return 0, err
	}
	if len(s) == 0 {
		return uintptr(unsafe.Pointer(unsafe.SliceData(m.b))) + uintptr(gpa), nil
	}
	return uintptr(unsafe.Pointer(&s[0])), nil
}

// Base returns the host address of guest physical address 0 and the RAM size.
func (m *Memory) Base() (uintptr, uint64) {
	return uintptr(unsafe.Pointer(unsafe.SliceData(m.b))), uint64(len(m.b))
}
