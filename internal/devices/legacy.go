package devices

import "time"

// CMOS emulates the MC146818 real-time clock at ports 0x70 (index) and
// 0x71 (data), reporting the host's current UTC time. Linux reads it once
// at boot to set the wall clock. Writes to the clock are ignored.
type CMOS struct {
	index uint8
	now   func() time.Time
}

// NewCMOS returns an RTC backed by the host clock.
func NewCMOS() *CMOS { return &CMOS{now: time.Now} }

// Read handles IN from 0x70 (offset 0) or 0x71 (offset 1).
func (c *CMOS) Read(offset uint16, data []byte) {
	if offset == 0 {
		fill(data, c.index)
		return
	}
	fill(data, c.register(c.index))
}

// Write handles OUT to 0x70 (offset 0) or 0x71 (offset 1).
func (c *CMOS) Write(offset uint16, data []byte) {
	if offset == 0 && len(data) > 0 {
		c.index = data[0] & 0x7f // bit 7 is the NMI mask
	}
}

func (c *CMOS) register(i uint8) uint8 {
	t := c.now().UTC()
	switch i {
	case 0x00:
		return bcd(t.Second())
	case 0x02:
		return bcd(t.Minute())
	case 0x04:
		return bcd(t.Hour())
	case 0x06:
		return bcd(int(t.Weekday()) + 1)
	case 0x07:
		return bcd(t.Day())
	case 0x08:
		return bcd(int(t.Month()))
	case 0x09:
		return bcd(t.Year() % 100)
	case 0x32:
		return bcd(t.Year() / 100)
	case 0x0a:
		return 0x26 // status A: update not in progress, 32.768 kHz base
	case 0x0b:
		return 0x02 // status B: 24-hour mode, BCD
	case 0x0d:
		return 0x80 // status D: battery good
	}
	return 0
}

func bcd(v int) uint8 { return uint8(v/10<<4 | v%10) }

// I8042 emulates just enough of the PS/2 keyboard controller at ports
// 0x60/0x64 for Linux to reboot the machine: the guest writes 0xFE (pulse
// reset line) to the command port. OnReset is then called.
type I8042 struct {
	OnReset func()
}

const (
	i8042StatusOBF = 0x01 // output buffer full (data waiting at 0x60)
	i8042StatusIBF = 0x02 // input buffer full (controller busy)
)

// Read handles IN from 0x60 (offset 0) or 0x64 (offset 4).
//
// The status register permanently reports OBF set and IBF clear. Linux's
// i8042 driver flushes the output buffer at probe time; a buffer that never
// empties makes it conclude within ~1 ms that there is no controller
// (instead of waiting ~0.5 s for replies that never come). The reboot path
// only waits for IBF to clear, so it can write the reset command at once.
func (k *I8042) Read(offset uint16, data []byte) {
	if offset == 4 {
		fill(data, i8042StatusOBF)
		return
	}
	fill(data, 0)
}

// Write handles OUT to 0x60 or 0x64.
func (k *I8042) Write(offset uint16, data []byte) {
	if offset == 4 && len(data) > 0 && data[0] == 0xfe && k.OnReset != nil {
		k.OnReset()
	}
}
