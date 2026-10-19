package devices

import (
	"bytes"
	"testing"
)

func in(s *Serial, off uint16) uint8 {
	b := []byte{0}
	s.Read(off, b)
	return b[0]
}

func out(s *Serial, off uint16, v uint8) { s.Write(off, []byte{v}) }

// Replays the checks Linux's 8250 driver does in autoconfig() to decide a
// UART is present and is a 16550A.
func TestSerialPassesLinuxProbe(t *testing.T) {
	s := NewSerial(&bytes.Buffer{}, nil)

	out(s, uartIER, 0)
	if v := in(s, uartIER) & 0x0f; v != 0 {
		t.Fatalf("IER after writing 0 = %#x", v)
	}
	out(s, uartIER, 0x0f)
	if v := in(s, uartIER) & 0x0f; v != 0x0f {
		t.Fatalf("IER after writing 0x0f = %#x", v)
	}
	out(s, uartIER, 0)

	out(s, uartMCR, mcrLoop|0x0a) // loopback with OUT2|RTS
	if v := in(s, uartMSR) & 0xf0; v != 0x90 {
		t.Fatalf("loopback MSR = %#x, want 0x90", v)
	}
	out(s, uartMCR, 0)

	out(s, uartFCR, 0x01) // enable FIFO
	if v := in(s, uartIIR) >> 6; v != 3 {
		t.Fatalf("IIR FIFO bits = %d, want 3 (16550A)", v)
	}

	out(s, uartSCR, 0xa5)
	if v := in(s, uartSCR); v != 0xa5 {
		t.Fatalf("scratch = %#x", v)
	}
}

func TestSerialTransmitAndInterrupts(t *testing.T) {
	var buf bytes.Buffer
	var edges []bool
	s := NewSerial(&buf, func(h bool) { edges = append(edges, h) })

	out(s, uartMCR, mcrOUT2)
	out(s, uartIER, ierTHRI) // enabling THRI with an empty THR raises the line
	if len(edges) != 1 || !edges[0] {
		t.Fatalf("edges after enabling THRI = %v", edges)
	}
	if v := in(s, uartIIR) & 0x0f; v != iirTHRI {
		t.Fatalf("IIR = %#x, want THRI", v)
	}
	if edges[len(edges)-1] {
		t.Fatal("reading IIR did not acknowledge the THRE interrupt")
	}

	out(s, uartTHR, 'K')
	if buf.String() != "K" {
		t.Fatalf("output = %q", buf.String())
	}
	if !edges[len(edges)-1] {
		t.Fatal("THRE interrupt not raised after transmit")
	}
}

func TestSerialReceive(t *testing.T) {
	s := NewSerial(&bytes.Buffer{}, nil)
	s.Receive([]byte("hi"))
	if in(s, uartLSR)&lsrDR == 0 {
		t.Fatal("LSR.DR not set")
	}
	if a, b := in(s, uartRBR), in(s, uartRBR); a != 'h' || b != 'i' {
		t.Fatalf("read %q%q", a, b)
	}
	if in(s, uartLSR)&lsrDR != 0 {
		t.Fatal("LSR.DR still set after draining")
	}
	s.Receive(make([]byte, rxBufSize+100))
	if len(s.rx) != rxBufSize {
		t.Fatalf("rx buffer grew to %d", len(s.rx))
	}
}
