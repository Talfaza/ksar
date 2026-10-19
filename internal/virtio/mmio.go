package virtio

import (
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// MMIOSize is the guest physical address space each device occupies.
const MMIOSize = 0x1000

// Feature bits common to all devices.
const (
	FeatureVersion1 = 1 << 32 // VIRTIO_F_VERSION_1: modern device
)

// Device status bits (spec 2.1).
const (
	statusAcknowledge = 1
	statusDriver      = 2
	statusDriverOK    = 4
	statusFeaturesOK  = 8
	statusNeedsReset  = 0x40
	statusFailed      = 0x80
)

// Interrupt status bits.
const (
	intUsedBuffer   = 1
	intConfigChange = 2
)

// MMIO register offsets (spec 4.2.2).
const (
	regMagic          = 0x000
	regVersion        = 0x004
	regDeviceID       = 0x008
	regVendorID       = 0x00c
	regDeviceFeatures = 0x010
	regDeviceFeatSel  = 0x014
	regDriverFeatures = 0x020
	regDriverFeatSel  = 0x024
	regQueueSel       = 0x030
	regQueueNumMax    = 0x034
	regQueueNum       = 0x038
	regQueueReady     = 0x044
	regQueueNotify    = 0x050
	regInterruptStat  = 0x060
	regInterruptACK   = 0x064
	regStatus         = 0x070
	regQueueDescLow   = 0x080
	regQueueDescHigh  = 0x084
	regQueueAvailLow  = 0x090
	regQueueAvailHigh = 0x094
	regQueueUsedLow   = 0x0a0
	regQueueUsedHigh  = 0x0a4
	regConfigGen      = 0x0fc
	regConfig         = 0x100

	magicValue = 0x74726976 // "virt"
	vendorID   = 0x5253414b // "KSAR"
)

// Device is a virtio device behind a transport.
type Device interface {
	// ID is the virtio device type (2 = block, 19 = vsock).
	ID() uint32
	// Features returns the device's feature bits, excluding
	// FeatureVersion1 which the transport always offers.
	Features() uint64
	// QueueSizes returns the maximum size of each queue.
	QueueSizes() []uint16
	// ReadConfig fills data from device config space at offset.
	ReadConfig(offset uint64, data []byte)
	// Activate starts the device once the driver set DRIVER_OK.
	Activate(a Activation) error
	// Notify is called when the driver kicks queue q.
	Notify(q int)
	// Reset stops the device and forgets all queue state.
	Reset()
}

// Activation is what a device receives when the driver is ready.
type Activation struct {
	Mem      *Memory
	Features uint64 // negotiated
	Queues   []QueueConfig
	// Interrupt tells the driver that used buffers are available. Safe to
	// call from any goroutine.
	Interrupt func()
}

type queueState struct {
	max   uint16
	cfg   QueueConfig
	ready bool
}

// MMIO is the virtio-mmio transport for one device.
type MMIO struct {
	dev   Device
	mem   *Memory
	irq   func(high bool)
	trace io.Writer

	mu          sync.Mutex
	devFeatSel  uint32
	drvFeatSel  uint32
	drvFeatures uint64
	queueSel    uint32
	queues      []queueState
	status      uint32
	intStatus   uint32
	irqHigh     bool
	active      bool
}

// NewMMIO wraps dev. irq drives the device's interrupt line.
func NewMMIO(dev Device, mem *Memory, irq func(high bool), trace io.Writer) *MMIO {
	m := &MMIO{dev: dev, mem: mem, irq: irq, trace: trace}
	m.resetLocked()
	return m
}

func (m *MMIO) offered() uint64 { return m.dev.Features() | FeatureVersion1 }

// Read handles a guest load from the device's MMIO window.
func (m *MMIO) Read(offset uint64, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if offset >= regConfig {
		m.dev.ReadConfig(offset-regConfig, data)
		return
	}
	if len(data) != 4 {
		clear(data) // registers are 32-bit only
		return
	}
	var v uint32
	switch offset {
	case regMagic:
		v = magicValue
	case regVersion:
		v = 2
	case regDeviceID:
		v = m.dev.ID()
	case regVendorID:
		v = vendorID
	case regDeviceFeatures:
		if m.devFeatSel < 2 {
			v = uint32(m.offered() >> (32 * m.devFeatSel))
		}
	case regQueueNumMax:
		if q := m.queue(); q != nil {
			v = uint32(q.max)
		}
	case regQueueReady:
		if q := m.queue(); q != nil && q.ready {
			v = 1
		}
	case regInterruptStat:
		v = m.intStatus
	case regStatus:
		v = m.status
	case regConfigGen:
		v = 0 // config space never changes
	}
	binary.LittleEndian.PutUint32(data, v)
}

// Write handles a guest store to the device's MMIO window.
func (m *MMIO) Write(offset uint64, data []byte) {
	if offset >= regConfig || len(data) != 4 {
		return // config space is read-only; registers are 32-bit only
	}
	v := binary.LittleEndian.Uint32(data)

	// QueueNotify is the hot path; don't hold the lock while the device
	// processes the queue (it may call Interrupt, which takes the lock).
	if offset == regQueueNotify {
		m.mu.Lock()
		ok := m.active && int(v) < len(m.queues)
		m.mu.Unlock()
		if ok {
			m.dev.Notify(int(v))
		}
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	switch offset {
	case regDeviceFeatSel:
		m.devFeatSel = v
	case regDriverFeatSel:
		m.drvFeatSel = v
	case regDriverFeatures:
		if m.status&statusFeaturesOK == 0 && m.drvFeatSel < 2 {
			shift := 32 * m.drvFeatSel
			m.drvFeatures = m.drvFeatures&^(0xffffffff<<shift) | uint64(v)<<shift
		}
	case regQueueSel:
		m.queueSel = v
	case regQueueNum:
		if q := m.queue(); q != nil && !q.ready {
			q.cfg.Size = uint16(min(v, uint32(q.max)))
		}
	case regQueueReady:
		if q := m.queue(); q != nil {
			q.ready = v == 1
		}
	case regQueueDescLow, regQueueDescHigh, regQueueAvailLow, regQueueAvailHigh,
		regQueueUsedLow, regQueueUsedHigh:
		if q := m.queue(); q != nil && !q.ready {
			setHalf(map[uint64]*uint64{
				regQueueDescLow: &q.cfg.Desc, regQueueDescHigh: &q.cfg.Desc,
				regQueueAvailLow: &q.cfg.Avail, regQueueAvailHigh: &q.cfg.Avail,
				regQueueUsedLow: &q.cfg.Used, regQueueUsedHigh: &q.cfg.Used,
			}[offset], offset%8 == 4, v)
		}
	case regInterruptACK:
		m.intStatus &^= v
		m.updateIRQLocked()
	case regStatus:
		m.writeStatusLocked(v)
	}
}

func setHalf(p *uint64, high bool, v uint32) {
	if high {
		*p = *p&0xffffffff | uint64(v)<<32
	} else {
		*p = *p&^0xffffffff | uint64(v)
	}
}

func (m *MMIO) queue() *queueState {
	if int(m.queueSel) < len(m.queues) {
		return &m.queues[m.queueSel]
	}
	return nil
}

func (m *MMIO) writeStatusLocked(v uint32) {
	if v == 0 {
		m.resetLocked()
		return
	}
	if v&statusFeaturesOK != 0 && m.status&statusFeaturesOK == 0 {
		// The driver may only accept what we offered, and must accept
		// VERSION_1 (we have no legacy interface).
		if m.drvFeatures&^m.offered() != 0 || m.drvFeatures&FeatureVersion1 == 0 {
			v &^= statusFeaturesOK
		}
	}
	m.status = v
	if v&statusDriverOK != 0 && !m.active && v&statusFailed == 0 {
		if err := m.activateLocked(); err != nil {
			m.logf("activate %s: %v", deviceName(m.dev.ID()), err)
			m.status |= statusNeedsReset
			m.intStatus |= intConfigChange
			m.updateIRQLocked()
		}
	}
}

func (m *MMIO) activateLocked() error {
	if m.status&statusFeaturesOK == 0 {
		return fmt.Errorf("driver set DRIVER_OK without FEATURES_OK")
	}
	a := Activation{Mem: m.mem, Features: m.drvFeatures, Interrupt: m.interrupt}
	for i, q := range m.queues {
		if !q.ready {
			// Unused queues are allowed; devices see Size 0.
			a.Queues = append(a.Queues, QueueConfig{})
			continue
		}
		if q.cfg.Size == 0 {
			return fmt.Errorf("queue %d enabled with size 0", i)
		}
		a.Queues = append(a.Queues, q.cfg)
	}
	if err := m.dev.Activate(a); err != nil {
		return err
	}
	m.active = true
	return nil
}

func (m *MMIO) resetLocked() {
	if m.active {
		m.dev.Reset()
	}
	sizes := m.dev.QueueSizes()
	m.queues = make([]queueState, len(sizes))
	for i, s := range sizes {
		m.queues[i].max = s
	}
	m.devFeatSel, m.drvFeatSel, m.drvFeatures = 0, 0, 0
	m.queueSel, m.status, m.intStatus = 0, 0, 0
	m.active = false
	m.updateIRQLocked()
}

// interrupt signals used buffers to the driver. Safe from any goroutine.
func (m *MMIO) interrupt() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.intStatus |= intUsedBuffer
	m.updateIRQLocked()
}

// updateIRQLocked drives the line from intStatus. The PIC is edge
// triggered: the line goes high when an interrupt becomes pending and low
// when the driver acknowledges everything, so each new batch is an edge.
func (m *MMIO) updateIRQLocked() {
	high := m.intStatus != 0
	if high != m.irqHigh {
		m.irqHigh = high
		if m.irq != nil {
			m.irq(high)
		}
	}
}

func (m *MMIO) logf(format string, args ...any) {
	if m.trace != nil {
		fmt.Fprintf(m.trace, "[ksar] virtio: "+format+"\n", args...)
	}
}

func deviceName(id uint32) string {
	switch id {
	case 2:
		return "block"
	case 19:
		return "vsock"
	}
	return fmt.Sprintf("device %d", id)
}
