package virtio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"
)

// Split virtqueue layout (virtio 1.x spec, section 2.7).
const (
	descSize = 16

	descFNext     = 1
	descFWrite    = 2
	descFIndirect = 4

	availFNoInterrupt = 1
)

// QueueConfig is what the driver programmed through the transport.
type QueueConfig struct {
	Size  uint16 // number of entries, a power of two
	Desc  uint64 // descriptor table address
	Avail uint64 // driver (available) ring address
	Used  uint64 // device (used) ring address
}

// Desc is one validated descriptor: Buf is the guest buffer it points to.
type Desc struct {
	Buf      []byte
	Writable bool // device may write (driver reads); otherwise device reads
}

// Queue is the device side of a split virtqueue.
type Queue struct {
	mem   *Memory
	size  uint16
	desc  []byte
	avail []byte // flags u16, idx u16, ring[size] u16, used_event u16
	used  []byte // flags u16, idx u16, ring[size]{id u32, len u32}, avail_event u16

	lastAvail uint16 // next avail ring entry to consume
	usedIdx   uint16 // next used ring entry to fill
	broken    error
}

// ErrBroken means the driver put the queue in an invalid state; the
// device stops using it until reset.
var ErrBroken = errors.New("virtio: queue is broken")

// NewQueue validates the driver's queue configuration and maps the rings.
func NewQueue(mem *Memory, c QueueConfig) (*Queue, error) {
	if c.Size == 0 || c.Size&(c.Size-1) != 0 {
		return nil, fmt.Errorf("virtio: queue size %d is not a power of two", c.Size)
	}
	if c.Desc%16 != 0 || c.Avail%2 != 0 || c.Used%4 != 0 {
		return nil, errors.New("virtio: misaligned queue")
	}
	n := uint64(c.Size)
	q := &Queue{mem: mem, size: c.Size}
	var err error
	if q.desc, err = mem.Slice(c.Desc, n*descSize); err != nil {
		return nil, err
	}
	if q.avail, err = mem.Slice(c.Avail, 6+2*n); err != nil {
		return nil, err
	}
	if q.used, err = mem.Slice(c.Used, 6+8*n); err != nil {
		return nil, err
	}
	return q, nil
}

func (q *Queue) fail(format string, args ...any) error {
	q.broken = fmt.Errorf("%w: "+format, append([]any{ErrBroken}, args...)...)
	return q.broken
}

// availIdx reads the driver's avail index. Reads are ordered on x86 and
// the atomic load keeps the compiler from caching it.
func (q *Queue) availIdx() uint16 {
	return loadU16(q.avail[2:])
}

// Pop returns the next available descriptor chain and its head index.
// ok is false when the driver has queued nothing new.
func (q *Queue) Pop() (head uint16, chain []Desc, ok bool, err error) {
	if q.broken != nil {
		return 0, nil, false, q.broken
	}
	idx := q.availIdx()
	pending := idx - q.lastAvail // uint16 arithmetic wraps correctly
	if pending == 0 {
		return 0, nil, false, nil
	}
	if pending > q.size {
		return 0, nil, false, q.fail("driver claims %d pending buffers in a %d-entry queue", pending, q.size)
	}
	slot := uint64(q.lastAvail % q.size)
	head = binary.LittleEndian.Uint16(q.avail[4+2*slot:])
	q.lastAvail++
	chain, err = q.walk(head)
	if err != nil {
		return 0, nil, false, err
	}
	return head, chain, true, nil
}

// walk follows a descriptor chain, validating every link and buffer.
func (q *Queue) walk(head uint16) ([]Desc, error) {
	var chain []Desc
	i := head
	seenWritable := false
	// A chain can't be longer than the table; a longer one has a loop.
	for steps := 0; ; steps++ {
		if steps >= int(q.size) {
			return nil, q.fail("descriptor chain loops or exceeds queue size")
		}
		if i >= q.size {
			return nil, q.fail("descriptor index %d out of range", i)
		}
		d := q.desc[uint64(i)*descSize:]
		addr := binary.LittleEndian.Uint64(d[0:])
		length := binary.LittleEndian.Uint32(d[8:])
		flags := binary.LittleEndian.Uint16(d[12:])
		next := binary.LittleEndian.Uint16(d[14:])
		if flags&descFIndirect != 0 {
			return nil, q.fail("indirect descriptors were not negotiated")
		}
		buf, err := q.mem.Slice(addr, uint64(length))
		if err != nil {
			return nil, q.fail("descriptor %d: %v", i, err)
		}
		w := flags&descFWrite != 0
		if !w && seenWritable {
			return nil, q.fail("readable descriptor after a writable one")
		}
		seenWritable = seenWritable || w
		chain = append(chain, Desc{Buf: buf, Writable: w})
		if flags&descFNext == 0 {
			return chain, nil
		}
		i = next
	}
}

// Push returns a chain to the driver, reporting how many bytes the device
// wrote into its writable buffers.
func (q *Queue) Push(head uint16, written uint32) {
	slot := uint64(q.usedIdx % q.size)
	e := q.used[4+8*slot:]
	binary.LittleEndian.PutUint32(e[0:], uint32(head))
	binary.LittleEndian.PutUint32(e[4:], written)
	q.usedIdx++
	// Publish the element before the index. The atomic store is a full
	// barrier on x86, so the driver never sees the new index early.
	storeU16(q.used[2:], q.usedIdx)
}

// NeedsInterrupt reports whether the driver wants an interrupt for used
// buffers (it can suppress them while polling).
func (q *Queue) NeedsInterrupt() bool {
	return binary.LittleEndian.Uint16(q.avail[0:])&availFNoInterrupt == 0
}

// loadU16 and storeU16 access a little-endian uint16 in guest memory
// atomically. They operate on the enclosing aligned 32-bit word, since Go
// has no 16-bit atomics; the ring layout guarantees idx fields are at
// offset 2 of a 4-byte aligned header when the ring itself is.
func loadU16(b []byte) uint16 {
	p := unsafe.Pointer(&b[0])
	if uintptr(p)%4 == 2 {
		w := atomic.LoadUint32((*uint32)(unsafe.Add(p, -2)))
		return uint16(w >> 16)
	}
	return binary.LittleEndian.Uint16(b)
}

func storeU16(b []byte, v uint16) {
	p := unsafe.Pointer(&b[0])
	if uintptr(p)%4 == 2 {
		w := (*uint32)(unsafe.Add(p, -2))
		for {
			old := atomic.LoadUint32(w)
			if atomic.CompareAndSwapUint32(w, old, old&0xffff|uint32(v)<<16) {
				return
			}
		}
	}
	binary.LittleEndian.PutUint16(b, v)
}
