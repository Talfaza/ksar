package vmm

import (
	"errors"
	"io"
	"sync"

	"ksar/internal/devices"
)

// maxChannelBuffer caps guest-to-host bytes waiting to be read. A guest
// that outruns the host by this much is cut off.
const maxChannelBuffer = 64 << 20

var errChannelOverflow = errors.New("agent channel: guest sent too much unread data")

// Channel is the host end of a serial port used as a byte stream to the
// guest agent. Writes block while the guest's receive buffer is full;
// reads block until the guest sends something. The vCPU thread never
// blocks on it.
type Channel struct {
	serial *devices.Serial

	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte // guest -> host, not yet read
	err    error  // set once closed
	closed chan struct{}
}

func newChannel() *Channel {
	c := &Channel{closed: make(chan struct{})}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// guestWriter is what the UART writes guest output into (vCPU thread).
type guestWriter struct{ c *Channel }

func (g guestWriter) Write(p []byte) (int, error) {
	c := g.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return len(p), nil // closed: drop
	}
	if len(c.buf)+len(p) > maxChannelBuffer {
		c.closeLocked(errChannelOverflow)
		return len(p), nil
	}
	c.buf = append(c.buf, p...)
	c.cond.Broadcast()
	return len(p), nil
}

// Read reads guest output. It returns io.EOF once the VM has stopped and
// all buffered data is read.
func (c *Channel) Read(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.buf) == 0 && c.err == nil {
		c.cond.Wait()
	}
	if len(c.buf) == 0 {
		return 0, c.err
	}
	n := copy(p, c.buf)
	c.buf = c.buf[n:]
	if len(c.buf) == 0 {
		c.buf = nil
	}
	return n, nil
}

// Write sends bytes to the guest, waiting for the guest to drain its
// receive buffer as needed.
func (c *Channel) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		select {
		case <-c.closed:
			return total - len(p), io.ErrClosedPipe
		default:
		}
		n := c.serial.Receive(p)
		p = p[n:]
		if len(p) == 0 {
			return total, nil
		}
		select {
		case <-c.serial.RxSpace():
		case <-c.closed:
			return total - len(p), io.ErrClosedPipe
		}
	}
	return total, nil
}

// close ends the stream; pending and future reads get err after the
// buffered data.
func (c *Channel) close(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeLocked(err)
}

func (c *Channel) closeLocked(err error) {
	if c.err != nil {
		return
	}
	c.err = err
	close(c.closed)
	c.cond.Broadcast()
}
