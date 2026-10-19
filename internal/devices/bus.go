package devices

import (
	"fmt"
	"io"
)

// PortDevice handles guest port I/O within its range. offset is relative
// to the device's base port. For reads the device fills data; for writes
// data holds what the guest wrote.
type PortDevice interface {
	Read(offset uint16, data []byte)
	Write(offset uint16, data []byte)
}

type portRange struct {
	base, size uint16
	dev        PortDevice
}

// PortBus routes port I/O to devices. Ports with no device behave like
// empty hardware: reads return all ones and writes are dropped.
type PortBus struct {
	ranges []portRange
	Trace  io.Writer // if set, unhandled accesses are logged here
}

// Register attaches dev to ports [base, base+size).
func (b *PortBus) Register(base, size uint16, dev PortDevice) error {
	for _, r := range b.ranges {
		if base < r.base+r.size && r.base < base+size {
			return fmt.Errorf("port range %#x+%d overlaps %#x+%d", base, size, r.base, r.size)
		}
	}
	b.ranges = append(b.ranges, portRange{base, size, dev})
	return nil
}

// Read dispatches a guest IN.
func (b *PortBus) Read(port uint16, data []byte) {
	if r := b.find(port); r != nil {
		r.dev.Read(port-r.base, data)
		return
	}
	fill(data, 0xff)
	b.trace("in", port, data)
}

// Write dispatches a guest OUT.
func (b *PortBus) Write(port uint16, data []byte) {
	if r := b.find(port); r != nil {
		r.dev.Write(port-r.base, data)
		return
	}
	b.trace("out", port, data)
}

func (b *PortBus) find(port uint16) *portRange {
	for i := range b.ranges {
		r := &b.ranges[i]
		if port >= r.base && port-r.base < r.size {
			return r
		}
	}
	return nil
}

func (b *PortBus) trace(dir string, port uint16, data []byte) {
	if b.Trace != nil {
		fmt.Fprintf(b.Trace, "[ksar] unhandled %s port %#x len %d data %x\n", dir, port, len(data), data)
	}
}
