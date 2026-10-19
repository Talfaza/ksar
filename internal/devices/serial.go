// Package devices contains the emulated hardware Ksar exposes to guests.
//
// Every byte a device receives comes from the untrusted guest. Devices keep
// fixed-size state, never index with guest-controlled values without
// masking, and never block.
package devices

import (
	"io"
	"sync"
)

// 8250/16550A UART register offsets from the base port.
const (
	uartRBR = 0 // read: receive buffer (DLAB=0)
	uartTHR = 0 // write: transmit holding (DLAB=0)
	uartDLL = 0 // divisor latch low (DLAB=1)
	uartIER = 1 // interrupt enable (DLAB=0)
	uartDLM = 1 // divisor latch high (DLAB=1)
	uartIIR = 2 // read: interrupt identification
	uartFCR = 2 // write: FIFO control
	uartLCR = 3 // line control
	uartMCR = 4 // modem control
	uartLSR = 5 // line status
	uartMSR = 6 // modem status
	uartSCR = 7 // scratch

	ierRDI  = 0x01 // received data available
	ierTHRI = 0x02 // transmit holding register empty

	iirNoInt = 0x01
	iirTHRI  = 0x02
	iirRDI   = 0x04
	iirFIFO  = 0xc0

	lcrDLAB = 0x80

	mcrDTR  = 0x01
	mcrRTS  = 0x02
	mcrOUT1 = 0x04
	mcrOUT2 = 0x08 // gates the IRQ line on PCs
	mcrLoop = 0x10

	lsrDR   = 0x01
	lsrTHRE = 0x20
	lsrTEMT = 0x40

	msrCTS = 0x10
	msrDSR = 0x20
	msrRI  = 0x40
	msrDCD = 0x80

	rxBufSize = 4096
)

// Serial emulates a 16550A UART. Transmitted bytes go to Out immediately,
// so the transmitter is always empty. Received bytes come from Receive.
type Serial struct {
	mu  sync.Mutex
	out io.Writer
	irq func(high bool) // sets the interrupt line level

	ier, lcr, mcr, scr, fcr uint8
	dll, dlm                uint8
	thrPending              bool // THRE interrupt pending
	irqHigh                 bool
	rx                      []byte
	err                     error
	rxSpace                 chan struct{} // signaled when the guest consumes input
}

// NewSerial returns a UART writing guest output to out and raising
// interrupts through irq.
func NewSerial(out io.Writer, irq func(high bool)) *Serial {
	return &Serial{out: out, irq: irq, dll: 12, // 9600 baud, as after reset
		rxSpace: make(chan struct{}, 1)}
}

// RxSpace returns a channel that receives a value whenever the guest has
// read input, i.e. when Receive may accept more bytes.
func (s *Serial) RxSpace() <-chan struct{} { return s.rxSpace }

// Err returns the first error writing guest output, if any.
func (s *Serial) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.err
}

// Receive queues bytes for the guest to read and returns how many fit in
// the receive buffer. Callers wanting to send more wait on RxSpace.
// Safe for concurrent use.
func (s *Serial) Receive(p []byte) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	room := rxBufSize - len(s.rx)
	if len(p) > room {
		p = p[:room]
	}
	if len(p) == 0 {
		return 0
	}
	s.rx = append(s.rx, p...)
	s.updateIRQ()
	return len(p)
}

// Read handles a guest IN from base+offset.
func (s *Serial) Read(offset uint16, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var v uint8
	switch offset & 7 {
	case uartRBR:
		if s.lcr&lcrDLAB != 0 {
			v = s.dll
		} else if len(s.rx) > 0 {
			v = s.rx[0]
			s.rx = s.rx[1:]
			s.updateIRQ()
			select {
			case s.rxSpace <- struct{}{}:
			default:
			}
		}
	case uartIER:
		if s.lcr&lcrDLAB != 0 {
			v = s.dlm
		} else {
			v = s.ier
		}
	case uartIIR:
		v = s.iir()
		if v&0x0f == iirTHRI {
			// Reading IIR acknowledges a THRE interrupt.
			s.thrPending = false
			s.updateIRQ()
		}
		if s.fcr&1 != 0 {
			v |= iirFIFO
		}
	case uartLCR:
		v = s.lcr
	case uartMCR:
		v = s.mcr
	case uartLSR:
		v = lsrTHRE | lsrTEMT
		if len(s.rx) > 0 {
			v |= lsrDR
		}
	case uartMSR:
		if s.mcr&mcrLoop != 0 {
			// Loopback: modem outputs feed modem inputs. Linux uses this to
			// verify a UART exists.
			if s.mcr&mcrRTS != 0 {
				v |= msrCTS
			}
			if s.mcr&mcrDTR != 0 {
				v |= msrDSR
			}
			if s.mcr&mcrOUT1 != 0 {
				v |= msrRI
			}
			if s.mcr&mcrOUT2 != 0 {
				v |= msrDCD
			}
		} else {
			v = msrDCD | msrDSR | msrCTS
		}
	case uartSCR:
		v = s.scr
	}
	fill(data, v)
}

// Write handles a guest OUT to base+offset.
func (s *Serial) Write(offset uint16, data []byte) {
	if len(data) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	v := data[0]
	switch offset & 7 {
	case uartTHR:
		if s.lcr&lcrDLAB != 0 {
			s.dll = v
			return
		}
		// Writing THR clears the THRE interrupt; the byte leaves instantly,
		// so it is raised again right after. The low-high transition gives
		// the edge-triggered PIC a fresh interrupt.
		s.thrPending = false
		s.updateIRQ()
		if s.mcr&mcrLoop != 0 {
			if len(s.rx) < rxBufSize {
				s.rx = append(s.rx, v)
			}
		} else if s.err == nil {
			_, s.err = s.out.Write([]byte{v})
		}
		s.thrPending = true
		s.updateIRQ()
	case uartIER:
		if s.lcr&lcrDLAB != 0 {
			s.dlm = v
			return
		}
		old := s.ier
		s.ier = v & 0x0f
		if s.ier&ierTHRI != 0 && old&ierTHRI == 0 {
			s.thrPending = true // transmitter is already empty
		}
		s.updateIRQ()
	case uartFCR:
		s.fcr = v & 0xc9
		if v&0x02 != 0 { // clear receive FIFO
			s.rx = s.rx[:0]
		}
		s.updateIRQ()
	case uartLCR:
		s.lcr = v
	case uartMCR:
		s.mcr = v & 0x1f
		s.updateIRQ()
	case uartSCR:
		s.scr = v
	}
}

// iir computes the interrupt identification value, highest priority first.
func (s *Serial) iir() uint8 {
	switch {
	case s.ier&ierRDI != 0 && len(s.rx) > 0:
		return iirRDI
	case s.ier&ierTHRI != 0 && s.thrPending:
		return iirTHRI
	}
	return iirNoInt
}

// updateIRQ drives the interrupt line from the current state. Callers hold mu.
func (s *Serial) updateIRQ() {
	high := s.iir() != iirNoInt && s.mcr&mcrOUT2 != 0
	if high != s.irqHigh {
		s.irqHigh = high
		if s.irq != nil {
			s.irq(high)
		}
	}
}

func fill(data []byte, v uint8) {
	for i := range data {
		data[i] = v
	}
}
