package virtio

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"syscall"
	"unsafe"
)

// virtio-vsock backed by the host kernel's vhost-vsock (spec 5.10). The
// kernel moves packets directly between guest memory and host sockets;
// the VMM only sets it up and relays kicks and interrupts.
const (
	vsockDeviceID = 19
	vsockQueueMax = 256
	vsockQueues   = 3 // rx, tx, event; vhost handles rx and tx
	vhostQueues   = 2
)

// vhost ioctls (<linux/vhost.h>), magic 0xAF.
func vhostIO(nr uintptr) uintptr        { return 0xAF<<8 | nr }
func vhostIOW(nr, size uintptr) uintptr { return 1<<30 | size<<16 | 0xAF<<8 | nr }
func vhostIOR(nr, size uintptr) uintptr { return 2<<30 | size<<16 | 0xAF<<8 | nr }

var (
	vhostGetFeatures  = vhostIOR(0x00, 8)
	vhostSetFeatures  = vhostIOW(0x00, 8)
	vhostSetOwner     = vhostIO(0x01)
	vhostSetMemTable  = vhostIOW(0x03, 8)  // struct vhost_memory header
	vhostSetVringNum  = vhostIOW(0x10, 8)  // struct vhost_vring_state
	vhostSetVringAddr = vhostIOW(0x11, 40) // struct vhost_vring_addr
	vhostSetVringBase = vhostIOW(0x12, 8)
	vhostSetVringKick = vhostIOW(0x20, 8) // struct vhost_vring_file
	vhostSetVringCall = vhostIOW(0x21, 8)
	vhostVsockSetCID  = vhostIOW(0x60, 8)
	vhostVsockSetRun  = vhostIOW(0x61, 4)
)

const (
	vhostDevice        = "/dev/vhost-vsock"
	efdCloexecNonblock = uintptr(syscall.O_CLOEXEC | syscall.O_NONBLOCK)
)

// Vsock is a vhost-vsock device. Each VM gets a unique context ID (CID)
// that the host uses to connect to it.
type Vsock struct {
	fd  int
	cid uint64

	mu      sync.Mutex
	running bool
	kick    [vhostQueues]int      // eventfds the VMM writes to kick vhost
	call    [vhostQueues]*os.File // eventfds vhost writes to interrupt
	stop    chan struct{}
	wg      sync.WaitGroup
}

// NewVsock opens vhost-vsock and claims a random free guest CID.
func NewVsock() (*Vsock, error) {
	fd, err := syscall.Open(vhostDevice, syscall.O_RDWR|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w (is the vhost_vsock module loaded?)", vhostDevice, err)
	}
	v := &Vsock{fd: fd}
	if err := v.ioctl(vhostSetOwner, 0); err != nil {
		v.Close()
		return nil, fmt.Errorf("VHOST_SET_OWNER: %w", err)
	}
	// CIDs are global on the host; pick random ones until one is free.
	for range 64 {
		cid := uint64(rand.Uint32N(1<<30)) + 3
		err := v.ioctlPtr(vhostVsockSetCID, unsafe.Pointer(&cid))
		if err == nil {
			v.cid = cid
			return v, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			v.Close()
			return nil, fmt.Errorf("VHOST_VSOCK_SET_GUEST_CID: %w", err)
		}
	}
	v.Close()
	return nil, errors.New("vsock: no free guest CID found")
}

// CID returns the guest's context ID.
func (v *Vsock) CID() uint32 { return uint32(v.cid) }

func (v *Vsock) ID() uint32 { return vsockDeviceID }

// Features offers only what the VMM-side ring setup supports: no event
// index or indirect descriptors, packed rings or mergeable buffers.
func (v *Vsock) Features() uint64 { return 0 }

func (v *Vsock) QueueSizes() []uint16 {
	return []uint16{vsockQueueMax, vsockQueueMax, vsockQueueMax}
}

// ReadConfig serves struct virtio_vsock_config { le64 guest_cid; }.
func (v *Vsock) ReadConfig(offset uint64, data []byte) {
	var cfg [8]byte
	binary.LittleEndian.PutUint64(cfg[:], v.cid)
	clear(data)
	if offset < uint64(len(cfg)) {
		copy(data, cfg[offset:])
	}
}

func (v *Vsock) Activate(a Activation) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.running {
		return errors.New("vsock: already active")
	}
	var hostFeatures uint64
	if err := v.ioctlPtr(vhostGetFeatures, unsafe.Pointer(&hostFeatures)); err != nil {
		return fmt.Errorf("VHOST_GET_FEATURES: %w", err)
	}
	features := a.Features & hostFeatures
	if features&FeatureVersion1 == 0 {
		return errors.New("vsock: vhost does not support VIRTIO_F_VERSION_1")
	}
	if err := v.ioctlPtr(vhostSetFeatures, unsafe.Pointer(&features)); err != nil {
		return fmt.Errorf("VHOST_SET_FEATURES: %w", err)
	}

	// One memory region: all of guest RAM.
	base, size := a.Mem.Base()
	memTable := struct {
		NRegions, _ uint32
		GPA, Size   uint64
		HVA, _      uint64
	}{NRegions: 1, GPA: 0, Size: size, HVA: uint64(base)}
	if err := v.ioctlPtr(vhostSetMemTable, unsafe.Pointer(&memTable)); err != nil {
		return fmt.Errorf("VHOST_SET_MEM_TABLE: %w", err)
	}

	v.stop = make(chan struct{})
	for i := range vhostQueues {
		if err := v.setupQueue(i, a); err != nil {
			v.teardownLocked()
			return fmt.Errorf("vsock queue %d: %w", i, err)
		}
	}
	running := int32(1)
	if err := v.ioctlPtr(vhostVsockSetRun, unsafe.Pointer(&running)); err != nil {
		v.teardownLocked()
		return fmt.Errorf("VHOST_VSOCK_SET_RUNNING: %w", err)
	}
	v.running = true
	return nil
}

func (v *Vsock) setupQueue(i int, a Activation) error {
	q := a.Queues[i]
	if q.Size == 0 {
		return errors.New("not configured")
	}
	// Validate the rings like any other queue before giving their host
	// addresses to the kernel.
	if _, err := NewQueue(a.Mem, q); err != nil {
		return err
	}
	n := uint64(q.Size)
	descHVA, _ := a.Mem.HostAddr(q.Desc, n*descSize)
	availHVA, _ := a.Mem.HostAddr(q.Avail, 6+2*n)
	usedHVA, _ := a.Mem.HostAddr(q.Used, 6+8*n)

	state := struct{ Index, Num uint32 }{uint32(i), uint32(q.Size)}
	if err := v.ioctlPtr(vhostSetVringNum, unsafe.Pointer(&state)); err != nil {
		return fmt.Errorf("VHOST_SET_VRING_NUM: %w", err)
	}
	state.Num = 0
	if err := v.ioctlPtr(vhostSetVringBase, unsafe.Pointer(&state)); err != nil {
		return fmt.Errorf("VHOST_SET_VRING_BASE: %w", err)
	}
	addr := struct {
		Index, Flags              uint32
		Desc, Used, Avail, LogGPA uint64
	}{Index: uint32(i), Desc: uint64(descHVA), Used: uint64(usedHVA), Avail: uint64(availHVA)}
	if err := v.ioctlPtr(vhostSetVringAddr, unsafe.Pointer(&addr)); err != nil {
		return fmt.Errorf("VHOST_SET_VRING_ADDR: %w", err)
	}

	kick, err := eventfd()
	if err != nil {
		return err
	}
	v.kick[i] = kick
	call, err := eventfd()
	if err != nil {
		return err
	}
	file := struct {
		Index uint32
		FD    int32
	}{uint32(i), int32(kick)}
	if err := v.ioctlPtr(vhostSetVringKick, unsafe.Pointer(&file)); err != nil {
		syscall.Close(call)
		return fmt.Errorf("VHOST_SET_VRING_KICK: %w", err)
	}
	file.FD = int32(call)
	if err := v.ioctlPtr(vhostSetVringCall, unsafe.Pointer(&file)); err != nil {
		syscall.Close(call)
		return fmt.Errorf("VHOST_SET_VRING_CALL: %w", err)
	}

	// Relay vhost's "used buffers" signals to the guest as interrupts.
	// The fd is non-blocking, so os.File uses the runtime poller and
	// Close wakes the reader.
	f := os.NewFile(uintptr(call), fmt.Sprintf("vsock-call-%d", i))
	v.call[i] = f
	v.wg.Add(1)
	go func() {
		defer v.wg.Done()
		var buf [8]byte
		for {
			if _, err := f.Read(buf[:]); err != nil {
				return
			}
			a.Interrupt()
		}
	}()
	return nil
}

// Notify forwards a guest kick to vhost.
func (v *Vsock) Notify(q int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.running || q >= vhostQueues {
		return // the event queue is unused
	}
	one := [8]byte{1}
	syscall.Write(v.kick[q], one[:])
}

// Reset stops vhost and closes the queues' eventfds. It does not wait for
// the interrupt relays to exit: the transport calls Reset with its lock
// held, and a relay may be blocked on that lock delivering an interrupt.
// Closing their fds makes them exit on their own.
func (v *Vsock) Reset() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.teardownLocked()
}

func (v *Vsock) teardownLocked() {
	if v.running {
		stopped := int32(0)
		v.ioctlPtr(vhostVsockSetRun, unsafe.Pointer(&stopped))
		v.running = false
	}
	for i := range vhostQueues {
		if v.call[i] != nil {
			v.call[i].Close()
			v.call[i] = nil
		}
		if v.kick[i] > 0 {
			syscall.Close(v.kick[i])
			v.kick[i] = 0
		}
	}
}

// Close stops the device, waits for its goroutines and releases the CID.
// It must not be called with the transport lock held.
func (v *Vsock) Close() error {
	v.mu.Lock()
	v.teardownLocked()
	v.mu.Unlock()
	v.wg.Wait()
	if v.fd >= 0 {
		err := syscall.Close(v.fd)
		v.fd = -1
		return err
	}
	return nil
}

func (v *Vsock) ioctl(req, arg uintptr) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(v.fd), req, arg)
	if errno != 0 {
		return errno
	}
	return nil
}

func (v *Vsock) ioctlPtr(req uintptr, p unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(v.fd), req, uintptr(p))
	if errno != 0 {
		return errno
	}
	return nil
}

func eventfd() (int, error) {
	fd, _, errno := syscall.Syscall(syscall.SYS_EVENTFD2, 0, efdCloexecNonblock, 0)
	if errno != 0 {
		return -1, fmt.Errorf("eventfd: %w", errno)
	}
	return int(fd), nil
}
