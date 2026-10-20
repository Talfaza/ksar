package virtio

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// The guest controls every byte of its RAM, so the fuzzer gets to choose
// the whole virtqueue: descriptor table, avail ring and indices. The
// device must never panic, read or write outside guest RAM, or loop.

const fuzzRAM = 64 << 10

// fuzzMemory lays fuzz data over a fixed-size RAM. A guard region after
// RAM catches any out-of-bounds write the bounds checks failed to stop.
func fuzzMemory(data []byte) (*Memory, []byte) {
	backing := make([]byte, fuzzRAM+4096)
	for i := fuzzRAM; i < len(backing); i++ {
		backing[i] = 0xEE
	}
	copy(backing[:fuzzRAM], data)
	return NewMemory(backing[:fuzzRAM:fuzzRAM]), backing
}

func checkGuard(t *testing.T, backing []byte) {
	for i := fuzzRAM; i < len(backing); i++ {
		if backing[i] != 0xEE {
			t.Fatalf("write outside guest RAM at offset %#x", i)
		}
	}
}

func FuzzQueue(f *testing.F) {
	// Seed: a valid one-descriptor chain.
	seed := make([]byte, 0x4000)
	binary.LittleEndian.PutUint64(seed[0x1000:], 0x3000) // desc 0 addr
	binary.LittleEndian.PutUint32(seed[0x1008:], 16)     // len
	binary.LittleEndian.PutUint16(seed[0x2002:], 1)      // avail idx
	f.Add(seed, uint16(8))
	f.Add([]byte{}, uint16(256))

	f.Fuzz(func(t *testing.T, data []byte, size uint16) {
		mem, backing := fuzzMemory(data)
		q, err := NewQueue(mem, QueueConfig{Size: size, Desc: 0x1000, Avail: 0x2000, Used: 0x2800})
		if err != nil {
			return
		}
		// Drain: Pop must terminate, since a chain or queue can't yield
		// more entries than the queue holds.
		for i := 0; i <= int(size)+1; i++ {
			head, chain, ok, err := q.Pop()
			if err != nil || !ok {
				break
			}
			for _, d := range chain {
				if d.Writable && len(d.Buf) > 0 {
					d.Buf[0] = 1
					d.Buf[len(d.Buf)-1] = 1
				}
			}
			q.Push(head, 1)
		}
		checkGuard(t, backing)
	})
}

func FuzzBlock(f *testing.F) {
	seed := make([]byte, 0x4000)
	// header at 0x3000 (IN, sector 0), data at 0x3100, status at 0x3400
	for i, d := range []struct {
		addr  uint64
		n     uint32
		flags uint16
	}{{0x3000, 16, descFNext}, {0x3100, 512, descFNext | descFWrite}, {0x3400, 1, descFWrite}} {
		p := seed[0x1000+16*i:]
		binary.LittleEndian.PutUint64(p, d.addr)
		binary.LittleEndian.PutUint32(p[8:], d.n)
		binary.LittleEndian.PutUint16(p[12:], d.flags)
		binary.LittleEndian.PutUint16(p[14:], uint16(i+1))
	}
	binary.LittleEndian.PutUint16(seed[0x2002:], 1)
	f.Add(seed, false)

	disk := filepath.Join(f.TempDir(), "disk.img")
	if err := os.WriteFile(disk, make([]byte, 8*blkSector), 0o644); err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, data []byte, readOnly bool) {
		b, err := NewBlock(disk, readOnly, "fuzz")
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		mem, backing := fuzzMemory(data)
		err = b.Activate(Activation{Mem: mem, Features: FeatureVersion1, Interrupt: func() {},
			Queues: []QueueConfig{{Size: 16, Desc: 0x1000, Avail: 0x2000, Used: 0x2800}}})
		if err != nil {
			return
		}
		b.Notify(0)
		b.Notify(0)
		checkGuard(t, backing)
	})
}

// FuzzMMIO drives the transport with arbitrary register accesses, the way
// a hostile guest driver could, with a block device behind it.
func FuzzMMIO(f *testing.F) {
	f.Add([]byte{0x70, 0, 0, 0, 1, 0, 0, 0}, []byte{})
	disk := filepath.Join(f.TempDir(), "disk.img")
	if err := os.WriteFile(disk, make([]byte, 8*blkSector), 0o644); err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, ops []byte, ram []byte) {
		b, err := NewBlock(disk, false, "fuzz")
		if err != nil {
			t.Fatal(err)
		}
		defer b.Close()
		mem, backing := fuzzMemory(ram)
		m := NewMMIO(b, mem, func(bool) {}, nil)
		// Each op: 2-byte offset, 4-byte value, 1 byte: bit0 write, bits1-3 size.
		for len(ops) >= 7 {
			off := uint64(binary.LittleEndian.Uint16(ops)) % MMIOSize
			val := ops[2:6]
			size := int(ops[6]>>1)%8 + 1
			buf := make([]byte, size)
			copy(buf, val)
			if ops[6]&1 != 0 {
				m.Write(off, buf)
			} else {
				m.Read(off, buf)
			}
			ops = ops[7:]
		}
		checkGuard(t, backing)
	})
}
