package virtio

import (
	"bytes"
	"os"
	"syscall"
	"testing"
	"time"
)

// newTestNet returns an active Net device, an RX and a TX fake driver
// sharing one guest RAM, and the proxy's end of the backend socket.
func newTestNet(t *testing.T) (*Net, *fakeDriver, *fakeDriver, *os.File, *int) {
	t.Helper()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	syscall.SetNonblock(fds[0], true)
	peer := os.NewFile(uintptr(fds[1]), "peer")
	t.Cleanup(func() { peer.Close() })
	n := NewNet(os.NewFile(uintptr(fds[0]), "net"), [6]byte{0x52, 0x54, 0, 1, 2, 3})
	t.Cleanup(func() { n.Close() })

	rx := newFakeDriver(t)
	tx := &fakeDriver{ram: rx.ram, mem: rx.mem, next: 0x80000,
		cfg: QueueConfig{Size: 16, Desc: 0x4000, Avail: 0x5000, Used: 0x6000}}
	irqs := new(int)
	err = n.Activate(Activation{Mem: rx.mem, Queues: []QueueConfig{rx.cfg, tx.cfg}, Interrupt: func() { *irqs++ }})
	if err != nil {
		t.Fatal(err)
	}
	return n, rx, tx, peer, irqs
}

func TestNetConfig(t *testing.T) {
	n, _, _, _, _ := newTestNet(t)
	b := make([]byte, 6)
	n.ReadConfig(0, b)
	if !bytes.Equal(b, []byte{0x52, 0x54, 0, 1, 2, 3}) {
		t.Errorf("mac = % x", b)
	}
	n.ReadConfig(100, b) // past the end reads as zeros
	if !bytes.Equal(b, make([]byte, 6)) {
		t.Errorf("config past the end = % x", b)
	}
}

func TestNetTransmit(t *testing.T) {
	n, _, tx, peer, irqs := newTestNet(t)
	frame := bytes.Repeat([]byte{0xab}, 60)
	// Header and frame in separate descriptors, as Linux may send them.
	hdr := tx.alloc(netHdrLen)
	data := tx.alloc(len(frame))
	copy(tx.ram[data:], frame)
	tx.chain([]buf{{hdr, netHdrLen, false}, {data, uint32(len(frame)), false}})
	// A hostile chain: a writable buffer in a TX request is dropped.
	w := tx.alloc(64)
	tx.chain([]buf{{w, 64, true}})
	n.Notify(netTX)

	got := make([]byte, 2000)
	peer.SetReadDeadline(time.Now().Add(time.Second))
	m, err := peer.Read(got)
	if err != nil || !bytes.Equal(got[:m], frame) {
		t.Fatalf("backend got % x (%v), want the frame without its header", got[:m], err)
	}
	if tx.usedIdx() != 2 || *irqs == 0 {
		t.Errorf("used idx %d, irqs %d: both requests must be completed", tx.usedIdx(), *irqs)
	}
	if n.TXDropped != 1 {
		t.Errorf("TXDropped = %d, want 1", n.TXDropped)
	}
}

func TestNetReceive(t *testing.T) {
	n, rx, _, peer, _ := newTestNet(t)
	frame := []byte("0123456789abcdefghijklmnopqrstuvwxyz0123456789abcdefghijklmn")

	// The frame arrives before the guest has buffers: it must wait, not drop.
	if _, err := peer.Write(frame); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if rx.usedIdx() != 0 {
		t.Fatal("delivered with no buffers")
	}
	// Two small buffers: the header and frame are split across them.
	a, b := rx.alloc(20), rx.alloc(100)
	rx.chain([]buf{{a, 20, true}, {b, 100, true}})
	n.Notify(netRX)

	deadline := time.Now().Add(time.Second)
	for rx.usedIdx() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if rx.usedIdx() != 1 {
		t.Fatal("frame not delivered after the RX kick")
	}
	if l := rx.usedLen(0); l != uint32(netHdrLen+len(frame)) {
		t.Errorf("used len %d, want %d", l, netHdrLen+len(frame))
	}
	got := append(append([]byte{}, rx.ram[a:a+20]...), rx.ram[b:b+100]...)
	if got[10] != 1 || got[11] != 0 {
		t.Errorf("num_buffers = %d, want 1", got[10])
	}
	if !bytes.Equal(got[netHdrLen:netHdrLen+len(frame)], frame) {
		t.Errorf("frame corrupted: %q", got[netHdrLen:netHdrLen+len(frame)])
	}

	// An oversized frame from the backend is dropped, not delivered.
	if _, err := peer.Write(make([]byte, MaxFrame+100)); err != nil {
		t.Fatal(err)
	}
	c := rx.alloc(2048)
	rx.chain([]buf{{c, 2048, true}})
	peer.Write(frame)
	n.Notify(netRX)
	for rx.usedIdx() == 1 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if l := rx.usedLen(1); l != uint32(netHdrLen+len(frame)) {
		t.Errorf("after an oversized frame, used len %d, want the next normal frame", l)
	}
}

func TestNetRequiresBothQueues(t *testing.T) {
	n := NewNet(os.NewFile(^uintptr(0), "none"), [6]byte{})
	defer n.Close()
	d := newFakeDriver(t)
	if err := n.Activate(Activation{Mem: d.mem, Queues: []QueueConfig{d.cfg, {}}}); err == nil {
		t.Error("activated without a TX queue")
	}
}
