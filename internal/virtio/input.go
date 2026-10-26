package virtio

import (
	"encoding/binary"
	"errors"
	"sync"
)

// virtio-input (spec 5.8): a keyboard or an absolute pointer (tablet) for
// the optional display. Events only flow host -> guest; what the guest
// sends back on the status queue (keyboard LEDs) is consumed and ignored.
const (
	inputDeviceID = 18
	inputQueueMax = 64

	inputCfgIDName   = 0x01
	inputCfgIDSerial = 0x02
	inputCfgIDDevids = 0x03
	inputCfgPropBits = 0x10
	inputCfgEvBits   = 0x11
	inputCfgAbsInfo  = 0x12

	inputEventQ  = 0
	inputStatusQ = 1

	// maxPending bounds events queued while the guest has no buffers
	// (e.g. before its driver is up). Older events are dropped first.
	maxPending = 1024
)

// Linux input event types and codes used here.
const (
	EvSyn = 0x00
	EvKey = 0x01
	EvRel = 0x02
	EvAbs = 0x03

	SynReport = 0
	AbsX      = 0x00
	AbsY      = 0x01
	RelHWheel = 0x06
	RelWheel  = 0x08
	BtnLeft   = 0x110
	BtnRight  = 0x111
	BtnMiddle = 0x112
)

// InputEvent is one evdev event.
type InputEvent struct {
	Type, Code uint16
	Value      int32
}

// AbsAxis describes an absolute axis.
type AbsAxis struct{ Min, Max int32 }

// InputSpec describes what an input device reports.
type InputSpec struct {
	Name    string
	Product uint16
	Bits    map[uint16][]uint16 // event type -> codes it can send
	Abs     map[uint16]AbsAxis
}

// KeyboardSpec is a keyboard with every key code up to KEY_MICMUTE.
func KeyboardSpec() InputSpec {
	var keys []uint16
	for k := uint16(1); k <= 248; k++ {
		keys = append(keys, k)
	}
	return InputSpec{Name: "Ksar Keyboard", Product: 1, Bits: map[uint16][]uint16{EvKey: keys}}
}

// TabletSpec is an absolute pointer covering a w x h screen, with three
// buttons and a wheel.
func TabletSpec(w, h int) InputSpec {
	return InputSpec{
		Name: "Ksar Tablet", Product: 2,
		Bits: map[uint16][]uint16{
			EvKey: {BtnLeft, BtnRight, BtnMiddle},
			EvRel: {RelHWheel, RelWheel},
			EvAbs: {AbsX, AbsY},
		},
		Abs: map[uint16]AbsAxis{AbsX: {0, int32(w - 1)}, AbsY: {0, int32(h - 1)}},
	}
}

// Input is a virtio-input device.
type Input struct {
	spec InputSpec

	// Config space selection, written by the driver (under the MMIO lock).
	sel, subsel uint8

	mu      sync.Mutex
	events  *Queue
	status  *Queue
	signal  func()
	pending []InputEvent
}

// NewInput creates an input device.
func NewInput(spec InputSpec) *Input { return &Input{spec: spec} }

func (in *Input) ID() uint32           { return inputDeviceID }
func (in *Input) Features() uint64     { return 0 }
func (in *Input) QueueSizes() []uint16 { return []uint16{inputQueueMax, inputQueueMax} }

func (in *Input) WriteConfig(offset uint64, data []byte) {
	for i, b := range data {
		switch offset + uint64(i) {
		case 0:
			in.sel = b
		case 1:
			in.subsel = b
		}
	}
}

// ReadConfig serves struct virtio_input_config: select, subsel, size,
// 5 reserved bytes, then 128 bytes of the selected data.
func (in *Input) ReadConfig(offset uint64, data []byte) {
	var cfg [136]byte
	cfg[0], cfg[1] = in.sel, in.subsel
	payload := in.configData()
	cfg[2] = byte(copy(cfg[8:], payload))
	clear(data)
	if offset < uint64(len(cfg)) {
		copy(data, cfg[offset:])
	}
}

func (in *Input) configData() []byte {
	switch in.sel {
	case inputCfgIDName:
		if in.subsel == 0 {
			return []byte(in.spec.Name)
		}
	case inputCfgIDSerial:
		if in.subsel == 0 {
			return []byte("ksar")
		}
	case inputCfgIDDevids:
		if in.subsel == 0 {
			b := make([]byte, 8)
			binary.LittleEndian.PutUint16(b[0:], 0x06) // BUS_VIRTUAL
			binary.LittleEndian.PutUint16(b[2:], 0x4b53)
			binary.LittleEndian.PutUint16(b[4:], in.spec.Product)
			binary.LittleEndian.PutUint16(b[6:], 1)
			return b
		}
	case inputCfgEvBits:
		codes := in.spec.Bits[uint16(in.subsel)]
		if len(codes) == 0 {
			return nil
		}
		var bm [128]byte
		top := 0
		for _, c := range codes {
			if int(c)/8 < len(bm) {
				bm[c/8] |= 1 << (c % 8)
				top = max(top, int(c)/8+1)
			}
		}
		return bm[:top]
	case inputCfgAbsInfo:
		a, ok := in.spec.Abs[uint16(in.subsel)]
		if !ok {
			return nil
		}
		b := make([]byte, 20) // min, max, fuzz, flat, res
		binary.LittleEndian.PutUint32(b[0:], uint32(a.Min))
		binary.LittleEndian.PutUint32(b[4:], uint32(a.Max))
		return b
	}
	return nil // PROP_BITS and anything unknown: empty
}

func (in *Input) Activate(a Activation) error {
	if len(a.Queues) != 2 || a.Queues[inputEventQ].Size == 0 {
		return errors.New("input: event queue not configured")
	}
	ev, err := NewQueue(a.Mem, a.Queues[inputEventQ])
	if err != nil {
		return err
	}
	var st *Queue
	if a.Queues[inputStatusQ].Size != 0 {
		if st, err = NewQueue(a.Mem, a.Queues[inputStatusQ]); err != nil {
			return err
		}
	}
	in.mu.Lock()
	in.events, in.status, in.signal = ev, st, a.Interrupt
	in.flushLocked()
	in.mu.Unlock()
	return nil
}

func (in *Input) Reset() {
	in.mu.Lock()
	in.events, in.status, in.signal = nil, nil, nil
	in.mu.Unlock()
}

func (in *Input) Notify(q int) {
	in.mu.Lock()
	defer in.mu.Unlock()
	switch q {
	case inputEventQ:
		in.flushLocked()
	case inputStatusQ:
		if in.status == nil {
			return
		}
		pushed := false
		for {
			head, _, ok, err := in.status.Pop()
			if err != nil || !ok {
				break
			}
			in.status.Push(head, 0)
			pushed = true
		}
		if pushed && in.status.NeedsInterrupt() {
			in.signal()
		}
	}
}

// Pending returns events not yet delivered (the guest had no buffers).
func (in *Input) Pending() []InputEvent {
	in.mu.Lock()
	defer in.mu.Unlock()
	return append([]InputEvent(nil), in.pending...)
}

// Send queues events for the guest; end each batch with a SYN_REPORT.
// Safe from any goroutine.
func (in *Input) Send(evs ...InputEvent) {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.pending = append(in.pending, evs...)
	if over := len(in.pending) - maxPending; over > 0 {
		in.pending = append(in.pending[:0], in.pending[over:]...)
	}
	in.flushLocked()
}

// flushLocked writes pending events into the guest's buffers.
func (in *Input) flushLocked() {
	if in.events == nil || len(in.pending) == 0 {
		return
	}
	n := 0
	for n < len(in.pending) {
		head, chain, ok, err := in.events.Pop()
		if err != nil || !ok {
			break
		}
		var b [8]byte
		e := in.pending[n]
		binary.LittleEndian.PutUint16(b[0:], e.Type)
		binary.LittleEndian.PutUint16(b[2:], e.Code)
		binary.LittleEndian.PutUint32(b[4:], uint32(e.Value))
		written, fit := scatter(chain, b[:])
		if !fit {
			written = 0 // a buffer too small for an event: drop the event
		}
		in.events.Push(head, written)
		n++
	}
	in.pending = append(in.pending[:0], in.pending[n:]...)
	if n > 0 && in.events.NeedsInterrupt() {
		in.signal()
	}
}
