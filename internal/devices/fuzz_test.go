package devices

import (
	"io"
	"testing"
)

// FuzzSerial feeds arbitrary port I/O to the UART, as a guest could.
func FuzzSerial(f *testing.F) {
	f.Add([]byte{0x01, 0x02, 0x83, 0x04, 0x08})
	f.Fuzz(func(t *testing.T, ops []byte) {
		s := NewSerial(io.Discard, func(bool) {})
		// Each op: offset (low 3 bits), direction (bit 3), then value.
		for len(ops) >= 2 {
			off := uint16(ops[0] & 7)
			if ops[0]&8 != 0 {
				s.Write(off, ops[1:2])
			} else {
				b := make([]byte, int(ops[1]%4)+1)
				s.Read(off, b)
			}
			if ops[0]&0x10 != 0 {
				s.Receive(ops[1:2])
			}
			ops = ops[2:]
		}
		if len(s.rx) > rxBufSize {
			t.Fatalf("receive buffer grew to %d", len(s.rx))
		}
	})
}

func FuzzPortBus(f *testing.F) {
	f.Add(uint16(0x3f8), []byte{1}, true)
	f.Fuzz(func(t *testing.T, port uint16, data []byte, write bool) {
		if len(data) > 8 {
			data = data[:8]
		}
		b := &PortBus{}
		b.Register(0x3f8, 8, NewSerial(io.Discard, nil))
		b.Register(0x70, 2, NewCMOS())
		b.Register(0x60, 5, &I8042{})
		if write {
			b.Write(port, data)
		} else {
			b.Read(port, data)
		}
	})
}
