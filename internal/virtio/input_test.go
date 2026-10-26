package virtio

import (
	"encoding/binary"
	"testing"
)

func readCfg(in *Input, sel, subsel byte) []byte {
	in.WriteConfig(0, []byte{sel, subsel})
	var hdr [3]byte
	in.ReadConfig(0, hdr[:])
	data := make([]byte, hdr[2])
	in.ReadConfig(8, data)
	return data
}

func TestInputConfig(t *testing.T) {
	tab := NewInput(TabletSpec(1280, 800))
	if got := string(readCfg(tab, inputCfgIDName, 0)); got != "Ksar Tablet" {
		t.Errorf("name = %q", got)
	}
	abs := readCfg(tab, inputCfgEvBits, EvAbs)
	if len(abs) != 1 || abs[0] != 0b11 {
		t.Errorf("abs bits = %08b", abs)
	}
	keys := readCfg(tab, inputCfgEvBits, EvKey)
	if len(keys) != BtnMiddle/8+1 || keys[BtnLeft/8]&(1<<(BtnLeft%8)) == 0 {
		t.Errorf("button bits wrong: % x", keys)
	}
	info := readCfg(tab, inputCfgAbsInfo, AbsY)
	if len(info) != 20 || binary.LittleEndian.Uint32(info[4:]) != 799 {
		t.Errorf("ABS_Y info = % x", info)
	}
	if got := readCfg(tab, inputCfgEvBits, 0x11); len(got) != 0 { // EV_LED
		t.Errorf("unsupported event type reported bits")
	}
	if got := readCfg(tab, 0x7f, 0); len(got) != 0 {
		t.Errorf("unknown select returned data")
	}
	kbd := NewInput(KeyboardSpec())
	if k := readCfg(kbd, inputCfgEvBits, EvKey); len(k) != 248/8+1 {
		t.Errorf("keyboard bitmap is %d bytes", len(k))
	}
}

func TestInputEvents(t *testing.T) {
	in := NewInput(KeyboardSpec())
	// Events before the driver is ready wait, bounded.
	for i := 0; i < maxPending+10; i++ {
		in.Send(InputEvent{Type: EvKey, Code: 30, Value: 1})
	}
	if n := len(in.Pending()); n != maxPending {
		t.Fatalf("pending = %d, want the cap %d", n, maxPending)
	}
	in.pending = nil

	d := newFakeDriver(t)
	st := &fakeDriver{ram: d.ram, mem: d.mem, cfg: QueueConfig{Size: 16, Desc: 0x4000, Avail: 0x5000, Used: 0x6000}}
	irqs := 0
	if err := in.Activate(Activation{Mem: d.mem, Queues: []QueueConfig{d.cfg, st.cfg}, Interrupt: func() { irqs++ }}); err != nil {
		t.Fatal(err)
	}
	bufs := []uint64{d.alloc(8), d.alloc(8)}
	for _, b := range bufs {
		d.chain([]buf{{b, 8, true}})
	}
	in.Send(InputEvent{Type: EvKey, Code: 30, Value: 1}, InputEvent{Type: EvSyn}, InputEvent{Type: EvKey, Code: 30, Value: 0})
	if d.usedIdx() != 2 || irqs == 0 {
		t.Fatalf("used %d, irqs %d", d.usedIdx(), irqs)
	}
	e := d.ram[bufs[0]:]
	if binary.LittleEndian.Uint16(e[0:]) != EvKey || binary.LittleEndian.Uint16(e[2:]) != 30 || binary.LittleEndian.Uint32(e[4:]) != 1 {
		t.Errorf("event = % x", e[:8])
	}
	if len(in.Pending()) != 1 {
		t.Fatalf("the third event should wait for a buffer")
	}
	b := d.alloc(8)
	d.chain([]buf{{b, 8, true}})
	in.Notify(inputEventQ)
	if d.usedIdx() != 3 || len(in.Pending()) != 0 {
		t.Errorf("refill did not deliver the waiting event")
	}
	// A buffer too small for an event: the event is dropped, not overflowed.
	small := d.alloc(4)
	d.chain([]buf{{small, 4, true}})
	in.Send(InputEvent{Type: EvKey, Code: 31, Value: 1})
	if d.usedLen(3) != 0 {
		t.Errorf("wrote %d bytes into a 4-byte buffer", d.usedLen(3))
	}
	// Status messages (LEDs) are consumed and returned.
	sb := st.alloc(8)
	st.chain([]buf{{sb, 8, false}})
	in.Notify(inputStatusQ)
	if st.usedIdx() != 1 {
		t.Errorf("status queue not drained")
	}
}
