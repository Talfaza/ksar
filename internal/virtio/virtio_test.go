package virtio

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeDriver plays the guest: it owns a RAM buffer and builds a split
// virtqueue in it the way a Linux driver would.
type fakeDriver struct {
	ram  []byte
	mem  *Memory
	cfg  QueueConfig
	next uint64 // bump allocator for data buffers
	desc uint16 // next free descriptor
	aidx uint16 // avail idx
}

func newFakeDriver(t *testing.T) *fakeDriver {
	ram := make([]byte, 1<<20)
	d := &fakeDriver{ram: ram, mem: NewMemory(ram), next: 0x10000}
	d.cfg = QueueConfig{Size: 16, Desc: 0x1000, Avail: 0x2000, Used: 0x3000}
	return d
}

func (d *fakeDriver) alloc(n int) uint64 {
	a := d.next
	d.next += uint64(n+15) &^ 15
	return a
}

// addDesc writes a raw descriptor and returns its index.
func (d *fakeDriver) addDesc(addr uint64, n uint32, flags, next uint16) uint16 {
	i := d.desc
	d.desc++
	p := d.ram[d.cfg.Desc+uint64(i)*16:]
	binary.LittleEndian.PutUint64(p[0:], addr)
	binary.LittleEndian.PutUint32(p[8:], n)
	binary.LittleEndian.PutUint16(p[12:], flags)
	binary.LittleEndian.PutUint16(p[14:], next)
	return i
}

// chain builds a descriptor chain from buffers and publishes it.
func (d *fakeDriver) chain(bufs []struct {
	addr     uint64
	n        uint32
	writable bool
}) uint16 {
	first := d.desc
	for i, b := range bufs {
		var flags uint16
		if b.writable {
			flags |= descFWrite
		}
		next := uint16(0)
		if i < len(bufs)-1 {
			flags |= descFNext
			next = d.desc + 1
		}
		d.addDesc(b.addr, b.n, flags, next)
	}
	d.publish(first)
	return first
}

func (d *fakeDriver) publish(head uint16) {
	slot := d.aidx % d.cfg.Size
	binary.LittleEndian.PutUint16(d.ram[d.cfg.Avail+4+2*uint64(slot):], head)
	d.aidx++
	storeU16(d.ram[d.cfg.Avail+2:], d.aidx) // as the device reads it: atomically
}

func (d *fakeDriver) usedIdx() uint16 {
	return loadU16(d.ram[d.cfg.Used+2:])
}

func (d *fakeDriver) usedLen(slot uint16) uint32 {
	return binary.LittleEndian.Uint32(d.ram[d.cfg.Used+4+8*uint64(slot)+4:])
}

type buf = struct {
	addr     uint64
	n        uint32
	writable bool
}

func TestQueueRejectsHostileChains(t *testing.T) {
	cases := map[string]func(d *fakeDriver){
		"buffer outside RAM": func(d *fakeDriver) {
			d.publish(d.addDesc(1<<40, 16, 0, 0))
		},
		"buffer runs past end of RAM": func(d *fakeDriver) {
			d.publish(d.addDesc(uint64(len(d.ram))-8, 16, 0, 0))
		},
		"chain loops": func(d *fakeDriver) {
			d.publish(d.addDesc(0x10000, 16, descFNext, 0)) // points at itself
		},
		"next index out of range": func(d *fakeDriver) {
			d.publish(d.addDesc(0x10000, 16, descFNext, 999))
		},
		"head index out of range": func(d *fakeDriver) {
			d.publish(500)
		},
		"indirect not negotiated": func(d *fakeDriver) {
			d.publish(d.addDesc(0x10000, 16, descFIndirect, 0))
		},
		"readable after writable": func(d *fakeDriver) {
			d.chain([]buf{{0x10000, 16, true}, {0x10100, 16, false}})
		},
		"avail idx jumps": func(d *fakeDriver) {
			binary.LittleEndian.PutUint16(d.ram[d.cfg.Avail+2:], 1000)
		},
	}
	for name, setup := range cases {
		d := newFakeDriver(t)
		q, err := NewQueue(d.mem, d.cfg)
		if err != nil {
			t.Fatal(err)
		}
		setup(d)
		_, _, ok, err := q.Pop()
		if ok || !errors.Is(err, ErrBroken) {
			t.Errorf("%s: ok=%v err=%v, want ErrBroken", name, ok, err)
			continue
		}
		// A broken queue stays broken.
		if _, _, ok, err := q.Pop(); ok || err == nil {
			t.Errorf("%s: queue recovered without reset", name)
		}
	}
}

func TestQueueRejectsBadConfig(t *testing.T) {
	d := newFakeDriver(t)
	for _, c := range []QueueConfig{
		{Size: 3, Desc: 0x1000, Avail: 0x2000, Used: 0x3000},                       // not a power of 2
		{Size: 16, Desc: 0x1001, Avail: 0x2000, Used: 0x3000},                      // misaligned
		{Size: 16, Desc: uint64(len(d.ram)) - 16, Avail: 0x2000, Used: 0x3000},     // desc past RAM
		{Size: 0x8000, Desc: 0x1000, Avail: 0x2000, Used: uint64(len(d.ram)) - 64}, // used past RAM
	} {
		if _, err := NewQueue(d.mem, c); err == nil {
			t.Errorf("NewQueue(%+v) succeeded", c)
		}
	}
}

func newTestDisk(t *testing.T, data []byte, ro bool) *Block {
	t.Helper()
	p := filepath.Join(t.TempDir(), "disk.img")
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := NewBlock(p, ro, "ksar-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

func activate(t *testing.T, dev Device, d *fakeDriver) *int {
	t.Helper()
	interrupts := new(int)
	if err := dev.Activate(Activation{Mem: d.mem, Features: FeatureVersion1,
		Queues: []QueueConfig{d.cfg}, Interrupt: func() { *interrupts++ }}); err != nil {
		t.Fatal(err)
	}
	return interrupts
}

// blkRequest queues one block request and returns where its data and
// status bytes live.
func blkRequest(d *fakeDriver, typ uint32, sector uint64, n int) (data, status uint64) {
	hdr := d.alloc(16)
	binary.LittleEndian.PutUint32(d.ram[hdr:], typ)
	binary.LittleEndian.PutUint64(d.ram[hdr+8:], sector)
	data = d.alloc(n)
	status = d.alloc(1)
	d.ram[status] = 0xff
	d.chain([]buf{{hdr, 16, false}, {data, uint32(n), typ == blkTIn || typ == blkTGetID}, {status, 1, true}})
	return data, status
}

func TestBlockReadWrite(t *testing.T) {
	disk := bytes.Repeat([]byte{0xAB}, 4*blkSector)
	copy(disk[blkSector:], "SECTOR-ONE")
	b := newTestDisk(t, disk, false)
	d := newFakeDriver(t)
	irqs := activate(t, b, d)

	// Read sector 1.
	data, status := blkRequest(d, blkTIn, 1, blkSector)
	b.Notify(0)
	if d.ram[status] != blkSOK {
		t.Fatalf("read status = %d", d.ram[status])
	}
	if !bytes.HasPrefix(d.ram[data:], []byte("SECTOR-ONE")) {
		t.Errorf("read data = %q", d.ram[data:data+10])
	}
	if d.usedIdx() != 1 || d.usedLen(0) != blkSector+1 || *irqs != 1 {
		t.Errorf("used idx=%d len=%d irqs=%d", d.usedIdx(), d.usedLen(0), *irqs)
	}

	// Write sector 2, then check the file.
	data, status = blkRequest(d, blkTOut, 2, blkSector)
	copy(d.ram[data:], "WRITTEN")
	b.Notify(0)
	if d.ram[status] != blkSOK {
		t.Fatalf("write status = %d", d.ram[status])
	}
	got := make([]byte, 7)
	b.f.ReadAt(got, 2*blkSector)
	if string(got) != "WRITTEN" {
		t.Errorf("disk sector 2 = %q", got)
	}

	// Out of range read fails with IOERR, not a crash or a short read.
	_, status = blkRequest(d, blkTIn, 4, blkSector)
	b.Notify(0)
	if d.ram[status] != blkSIOErr {
		t.Errorf("out-of-range status = %d, want IOERR", d.ram[status])
	}
	// Huge sector numbers must not overflow the bounds check.
	_, status = blkRequest(d, blkTIn, 1<<62, blkSector)
	b.Notify(0)
	if d.ram[status] != blkSIOErr {
		t.Errorf("overflowing sector status = %d, want IOERR", d.ram[status])
	}

	// GET_ID.
	data, status = blkRequest(d, blkTGetID, 0, blkIDLen)
	b.Notify(0)
	if d.ram[status] != blkSOK || !bytes.HasPrefix(d.ram[data:], []byte("ksar-test")) {
		t.Errorf("get id status=%d id=%q", d.ram[status], d.ram[data:data+blkIDLen])
	}
}

func TestBlockReadOnly(t *testing.T) {
	b := newTestDisk(t, make([]byte, 2*blkSector), true)
	if b.Features()&blkFRO == 0 {
		t.Error("read-only disk does not advertise VIRTIO_BLK_F_RO")
	}
	d := newFakeDriver(t)
	activate(t, b, d)
	data, status := blkRequest(d, blkTOut, 0, blkSector)
	copy(d.ram[data:], "EVIL")
	b.Notify(0)
	if d.ram[status] != blkSIOErr {
		t.Errorf("write to read-only disk: status %d", d.ram[status])
	}
	got := make([]byte, 4)
	b.f.ReadAt(got, 0)
	if string(got) == "EVIL" {
		t.Fatal("read-only disk was modified")
	}
}

// Drive the MMIO transport through the same handshake Linux does.
func TestMMIOHandshake(t *testing.T) {
	b := newTestDisk(t, make([]byte, 8*blkSector), false)
	d := newFakeDriver(t)
	var irq []bool
	m := NewMMIO(b, d.mem, func(h bool) { irq = append(irq, h) }, nil)

	r := func(off uint64) uint32 {
		var v [4]byte
		m.Read(off, v[:])
		return binary.LittleEndian.Uint32(v[:])
	}
	w := func(off uint64, v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		m.Write(off, b[:])
	}
	if r(regMagic) != magicValue || r(regVersion) != 2 || r(regDeviceID) != blkDeviceID {
		t.Fatal("bad identification registers")
	}
	w(regStatus, statusAcknowledge|statusDriver)
	w(regDeviceFeatSel, 1)
	if r(regDeviceFeatures)&1 == 0 {
		t.Fatal("VERSION_1 not offered")
	}

	// Accepting a feature that was not offered must fail FEATURES_OK.
	w(regDriverFeatSel, 0)
	w(regDriverFeatures, 1<<31)
	w(regDriverFeatSel, 1)
	w(regDriverFeatures, 1)
	w(regStatus, statusAcknowledge|statusDriver|statusFeaturesOK)
	if r(regStatus)&statusFeaturesOK != 0 {
		t.Fatal("FEATURES_OK accepted unknown feature")
	}
	w(regStatus, 0) // reset

	w(regStatus, statusAcknowledge|statusDriver)
	w(regDriverFeatSel, 1)
	w(regDriverFeatures, 1)
	w(regStatus, statusAcknowledge|statusDriver|statusFeaturesOK)
	if r(regStatus)&statusFeaturesOK == 0 {
		t.Fatal("FEATURES_OK rejected")
	}
	w(regQueueSel, 0)
	if r(regQueueNumMax) != blkQueueMax {
		t.Fatal("bad QueueNumMax")
	}
	w(regQueueNum, uint32(d.cfg.Size))
	w(regQueueDescLow, uint32(d.cfg.Desc))
	w(regQueueAvailLow, uint32(d.cfg.Avail))
	w(regQueueUsedLow, uint32(d.cfg.Used))
	w(regQueueReady, 1)
	w(regStatus, statusAcknowledge|statusDriver|statusFeaturesOK|statusDriverOK)
	if r(regStatus)&statusNeedsReset != 0 {
		t.Fatal("activation failed")
	}

	_, status := blkRequest(d, blkTIn, 0, blkSector)
	w(regQueueNotify, 0)
	if d.ram[status] != blkSOK {
		t.Fatalf("status = %d", d.ram[status])
	}
	if r(regInterruptStat) != intUsedBuffer || len(irq) != 1 || !irq[0] {
		t.Fatalf("interrupt status=%d irq=%v", r(regInterruptStat), irq)
	}
	w(regInterruptACK, intUsedBuffer)
	if len(irq) != 2 || irq[1] {
		t.Fatalf("irq not lowered after ACK: %v", irq)
	}
}

func TestMMIOBadQueueAddressFailsSafely(t *testing.T) {
	b := newTestDisk(t, make([]byte, blkSector), false)
	d := newFakeDriver(t)
	m := NewMMIO(b, d.mem, func(bool) {}, nil)
	w := func(off uint64, v uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], v)
		m.Write(off, b[:])
	}
	w(regStatus, statusAcknowledge|statusDriver)
	w(regDriverFeatSel, 1)
	w(regDriverFeatures, 1)
	w(regStatus, statusAcknowledge|statusDriver|statusFeaturesOK)
	w(regQueueNum, 16)
	w(regQueueDescHigh, 0x10) // way outside RAM
	w(regQueueReady, 1)
	w(regStatus, statusAcknowledge|statusDriver|statusFeaturesOK|statusDriverOK)
	var st [4]byte
	m.Read(regStatus, st[:])
	if binary.LittleEndian.Uint32(st[:])&statusNeedsReset == 0 {
		t.Fatal("device activated with a queue outside guest RAM")
	}
	w(regQueueNotify, 0) // must be ignored, not crash
}
