package virtio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"sync"
)

// virtio-blk (spec 5.2).
const (
	blkDeviceID = 2
	blkSector   = 512
	blkQueueMax = 256

	blkFSegMax = 1 << 2
	blkFRO     = 1 << 5
	blkFFlush  = 1 << 9

	blkTIn    = 0
	blkTOut   = 1
	blkTFlush = 4
	blkTGetID = 8

	blkSOK     = 0
	blkSIOErr  = 1
	blkSUnsupp = 2

	blkIDLen = 20
)

// Block is a virtio-blk device backed by a host file.
type Block struct {
	f        *os.File
	readOnly bool
	sectors  uint64
	id       string

	mu     sync.Mutex
	q      *Queue
	signal func()
}

// NewBlock opens path as a disk. The file size must be a multiple of 512.
func NewBlock(path string, readOnly bool, id string) (*Block, error) {
	flag := os.O_RDWR
	if readOnly {
		flag = os.O_RDONLY
	}
	f, err := os.OpenFile(path, flag, 0)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size()%blkSector != 0 {
		f.Close()
		return nil, fmt.Errorf("disk %s: size %d is not a multiple of %d", path, st.Size(), blkSector)
	}
	return &Block{f: f, readOnly: readOnly, sectors: uint64(st.Size()) / blkSector, id: id}, nil
}

// Close closes the backing file.
func (b *Block) Close() error { return b.f.Close() }

func (b *Block) ID() uint32 { return blkDeviceID }

func (b *Block) Features() uint64 {
	f := uint64(blkFSegMax | blkFFlush)
	if b.readOnly {
		f |= blkFRO
	}
	return f
}

func (b *Block) QueueSizes() []uint16 { return []uint16{blkQueueMax} }

// ReadConfig serves struct virtio_blk_config: capacity (u64 sectors) and
// seg_max (u32) are all the driver needs.
func (b *Block) ReadConfig(offset uint64, data []byte) {
	var cfg [16]byte
	binary.LittleEndian.PutUint64(cfg[0:], b.sectors)
	binary.LittleEndian.PutUint32(cfg[12:], blkQueueMax-2) // seg_max
	clear(data)
	if offset < uint64(len(cfg)) {
		copy(data, cfg[offset:])
	}
}

func (b *Block) Activate(a Activation) error {
	if len(a.Queues) != 1 || a.Queues[0].Size == 0 {
		return errors.New("block: request queue not configured")
	}
	q, err := NewQueue(a.Mem, a.Queues[0])
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.q, b.signal = q, a.Interrupt
	b.mu.Unlock()
	return nil
}

func (b *Block) Reset() {
	b.mu.Lock()
	b.q, b.signal = nil, nil
	b.mu.Unlock()
}

// Notify processes every queued request. It runs on the vCPU thread that
// kicked the queue, so requests complete before the guest continues.
func (b *Block) Notify(int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.q == nil {
		return
	}
	pushed := false
	for {
		head, chain, ok, err := b.q.Pop()
		if err != nil || !ok {
			break // a broken queue stays broken until the driver resets
		}
		b.q.Push(head, b.handle(chain))
		pushed = true
	}
	if pushed && b.q.NeedsInterrupt() {
		b.signal()
	}
}

// handle executes one request and returns the bytes written to the guest.
// Layout: header (readable, 16 bytes) | data buffers | status (writable, 1 byte).
func (b *Block) handle(chain []Desc) uint32 {
	if len(chain) < 2 || chain[0].Writable || len(chain[0].Buf) < 16 {
		return 0 // malformed: no place to report status
	}
	last := chain[len(chain)-1]
	if !last.Writable || len(last.Buf) < 1 {
		return 0
	}
	hdr := chain[0].Buf
	typ := binary.LittleEndian.Uint32(hdr[0:])
	sector := binary.LittleEndian.Uint64(hdr[8:])
	data := chain[1 : len(chain)-1]
	status := &last.Buf[0]

	written := uint32(1) // the status byte
	switch typ {
	case blkTIn:
		n, err := b.transfer(data, sector, true)
		written += n
		*status = statusFor(err)
	case blkTOut:
		if b.readOnly {
			*status = blkSIOErr
			break
		}
		_, err := b.transfer(data, sector, false)
		*status = statusFor(err)
	case blkTFlush:
		var err error
		if !b.readOnly {
			err = b.f.Sync()
		}
		*status = statusFor(err)
	case blkTGetID:
		if len(data) != 1 || !data[0].Writable {
			*status = blkSIOErr
			break
		}
		n := copy(data[0].Buf[:min(len(data[0].Buf), blkIDLen)], b.id)
		clear(data[0].Buf[n:min(len(data[0].Buf), blkIDLen)])
		written += uint32(min(len(data[0].Buf), blkIDLen))
		*status = blkSOK
	default:
		*status = blkSUnsupp
	}
	return written
}

// transfer reads (in=true) or writes the data buffers at sector.
func (b *Block) transfer(data []Desc, sector uint64, in bool) (uint32, error) {
	var total uint64
	for _, d := range data {
		if d.Writable != in {
			return 0, errors.New("buffer direction does not match request")
		}
		total += uint64(len(d.Buf))
	}
	if total%blkSector != 0 || sector > b.sectors || total/blkSector > b.sectors-sector {
		return 0, errors.New("request outside the disk")
	}
	off := int64(sector * blkSector)
	var n uint32
	for _, d := range data {
		var err error
		var m int
		if in {
			m, err = b.f.ReadAt(d.Buf, off)
		} else {
			m, err = b.f.WriteAt(d.Buf, off)
		}
		n += uint32(m)
		if err != nil {
			return n, err
		}
		off += int64(m)
	}
	if !in {
		n = 0 // nothing written into guest memory
	}
	return n, nil
}

func statusFor(err error) byte {
	if err != nil {
		return blkSIOErr
	}
	return blkSOK
}
