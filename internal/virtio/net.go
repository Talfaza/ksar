package virtio

import (
	"encoding/binary"
	"errors"
	"os"
	"sync"
	"syscall"
)

// virtio-net (spec 5.1), the minimal form: one RX and one TX queue, no
// checksum or segmentation offloads, a fixed MAC. Ethernet frames go to
// and from a backend socket (SOCK_SEQPACKET: one message per frame),
// whose other end is Ksar's network proxy process.
const (
	netDeviceID = 1
	netQueueMax = 256

	netFMAC = 1 << 5

	// With VIRTIO_F_VERSION_1 every frame carries a 12-byte header
	// (struct virtio_net_hdr_mrg_rxbuf). With no offloads negotiated it
	// is all zeros except num_buffers, which is 1.
	netHdrLen = 12

	// MaxFrame is the largest Ethernet frame passed in either direction.
	// The guest's MTU is 1500 and no segmentation offload is offered, so
	// real frames are at most 1514 bytes; anything larger is dropped.
	MaxFrame = 1514

	netRX = 0
	netTX = 1
)

// Net is a virtio-net device bridged to a backend socket.
type Net struct {
	backend *os.File
	mac     [6]byte

	rxMu   sync.Mutex
	rx     *Queue
	txMu   sync.Mutex
	tx     *Queue
	signal func()

	kick      chan struct{} // the driver added RX buffers
	closed    chan struct{}
	closeOnce sync.Once

	// Counters, for diagnostics. Guarded by the queue locks.
	TXDropped, RXDropped uint64
}

// NewNet creates the device. backend must be a non-blocking SOCK_SEQPACKET
// socket; the device owns it from now on.
func NewNet(backend *os.File, mac [6]byte) *Net {
	n := &Net{
		backend: backend, mac: mac,
		kick:   make(chan struct{}, 1),
		closed: make(chan struct{}),
	}
	go n.receive()
	return n
}

// Close stops the device and closes the backend.
func (n *Net) Close() error {
	var err error
	n.closeOnce.Do(func() {
		close(n.closed)
		err = n.backend.Close()
	})
	return err
}

func (n *Net) ID() uint32           { return netDeviceID }
func (n *Net) Features() uint64     { return netFMAC }
func (n *Net) QueueSizes() []uint16 { return []uint16{netQueueMax, netQueueMax} }

// ReadConfig serves struct virtio_net_config: the MAC, then status, which
// is only defined with VIRTIO_NET_F_STATUS (not offered) and reads as 0.
func (n *Net) ReadConfig(offset uint64, data []byte) {
	var cfg [8]byte
	copy(cfg[:], n.mac[:])
	clear(data)
	if offset < uint64(len(cfg)) {
		copy(data, cfg[offset:])
	}
}

func (n *Net) Activate(a Activation) error {
	if len(a.Queues) != 2 || a.Queues[netRX].Size == 0 || a.Queues[netTX].Size == 0 {
		return errors.New("net: RX and TX queues must both be configured")
	}
	rx, err := NewQueue(a.Mem, a.Queues[netRX])
	if err != nil {
		return err
	}
	tx, err := NewQueue(a.Mem, a.Queues[netTX])
	if err != nil {
		return err
	}
	n.rxMu.Lock()
	n.txMu.Lock()
	n.rx, n.tx, n.signal = rx, tx, a.Interrupt
	n.txMu.Unlock()
	n.rxMu.Unlock()
	n.wake()
	return nil
}

func (n *Net) Reset() {
	n.rxMu.Lock()
	n.txMu.Lock()
	n.rx, n.tx, n.signal = nil, nil, nil
	n.txMu.Unlock()
	n.rxMu.Unlock()
}

func (n *Net) wake() {
	select {
	case n.kick <- struct{}{}:
	default:
	}
}

// Notify handles a kick. RX kicks mean new receive buffers; TX kicks are
// processed right here on the vCPU thread.
func (n *Net) Notify(q int) {
	switch q {
	case netRX:
		n.wake()
	case netTX:
		n.transmit()
	}
}

// transmit sends every queued TX frame to the backend. It never blocks the
// vCPU: if the backend is full the frame is dropped, as a real NIC would.
func (n *Net) transmit() {
	n.txMu.Lock()
	defer n.txMu.Unlock()
	if n.tx == nil {
		return
	}
	var frame [netHdrLen + MaxFrame]byte
	pushed := false
	for {
		head, chain, ok, err := n.tx.Pop()
		if err != nil || !ok {
			break // a broken queue stays broken until the driver resets
		}
		if size, ok := gather(frame[:], chain); ok && size > netHdrLen {
			if !n.send(frame[netHdrLen:size]) {
				n.TXDropped++
			}
		} else {
			n.TXDropped++
		}
		n.tx.Push(head, 0)
		pushed = true
	}
	if pushed && n.tx.NeedsInterrupt() {
		n.signal()
	}
}

// gather copies a TX chain (all readable) into buf. It fails if the chain
// has writable buffers or holds more than fits.
func gather(buf []byte, chain []Desc) (int, bool) {
	size := 0
	for _, d := range chain {
		if d.Writable || len(d.Buf) > len(buf)-size {
			return 0, false
		}
		size += copy(buf[size:], d.Buf)
	}
	return size, true
}

// send writes one frame without waiting.
func (n *Net) send(frame []byte) bool {
	rc, err := n.backend.SyscallConn()
	if err != nil {
		return false
	}
	sent := false
	rc.Write(func(fd uintptr) bool {
		_, err := syscall.Write(int(fd), frame)
		sent = err == nil
		return true // never wait for the socket to drain
	})
	return sent
}

// receive moves frames from the backend into the guest's RX buffers. When
// the guest has no free buffers it waits for the driver to add some, which
// pushes back on the proxy instead of dropping.
func (n *Net) receive() {
	buf := make([]byte, MaxFrame+1)
	for {
		m, err := n.backend.Read(buf)
		if err != nil {
			return // backend closed: the proxy exited or the VM is gone
		}
		if m == 0 || m > MaxFrame {
			continue // truncated or empty: not a frame we accept
		}
		for !n.deliver(buf[:m]) {
			select {
			case <-n.kick:
			case <-n.closed:
				return
			}
		}
	}
}

// deliver puts one frame into the next RX buffer. It reports false if the
// guest has none free (try again after a kick). A frame that cannot be
// delivered at all (device reset, broken queue, buffer too small) is
// dropped and reported as delivered.
func (n *Net) deliver(frame []byte) bool {
	n.rxMu.Lock()
	defer n.rxMu.Unlock()
	if n.rx == nil {
		n.RXDropped++
		return true
	}
	head, chain, ok, err := n.rx.Pop()
	if err != nil {
		n.RXDropped++
		return true
	}
	if !ok {
		return false
	}
	var hdr [netHdrLen]byte
	binary.LittleEndian.PutUint16(hdr[10:], 1) // num_buffers
	written, fit := scatter(chain, hdr[:], frame)
	if !fit {
		n.RXDropped++
		written = 0
	}
	n.rx.Push(head, written)
	if n.rx.NeedsInterrupt() {
		n.signal()
	}
	return true
}

// scatter writes hdr then frame across an RX chain's buffers, all of which
// must be writable.
func scatter(chain []Desc, parts ...[]byte) (uint32, bool) {
	var written uint32
	i, off := 0, 0
	for _, p := range parts {
		for len(p) > 0 {
			if i >= len(chain) || !chain[i].Writable {
				return written, false
			}
			c := copy(chain[i].Buf[off:], p)
			p, off, written = p[c:], off+c, written+uint32(c)
			if off == len(chain[i].Buf) {
				i, off = i+1, 0
			}
		}
	}
	return written, true
}
