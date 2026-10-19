package vmm

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"ksar/internal/boot"
	"ksar/internal/devices"
	"ksar/internal/kvm"
)

// DefaultCmdline is the kernel command line used when none is given.
//
//   - console=ttyS0, earlyprintk: kernel log on our emulated COM1
//   - reboot=k: reboot through the keyboard controller, which Ksar treats
//     as "the guest is done"
//   - panic=-1: reboot immediately on panic instead of hanging
//   - pci=off, acpi=off: there is no PCI bus or ACPI table to find
//   - 8250.nr_uarts=2: COM1 (console) and COM2 (agent channel) only
const DefaultCmdline = "console=ttyS0 earlyprintk=serial,ttyS0,115200 reboot=k panic=-1 pci=off acpi=off 8250.nr_uarts=2"

// tssAddr is the 3-page region Intel VMX needs, placed just under the
// 4 GiB boundary, far from guest RAM (capped at 3 GiB).
const tssAddr = 0xfffbd000

// consoleTailSize is how much recent console output is kept for error
// reports.
const consoleTailSize = 64 << 10

// LinuxConfig describes a Linux microVM.
type LinuxConfig struct {
	Kernel       []byte // bzImage
	Initrd       []byte // optional initramfs
	Cmdline      string // defaults to DefaultCmdline
	MemSize      int    // guest RAM in bytes (default 256 MiB)
	Timeout      time.Duration
	Output       io.Writer // serial console output (may be nil)
	MaxOutput    int64     // cap on console bytes forwarded to Output (default 16 MiB)
	Trace        io.Writer // if set, unhandled port/MMIO accesses are logged
	AgentChannel bool      // add COM2 as a byte stream to the guest agent
}

// LinuxResult reports how a Linux guest run ended.
type LinuxResult struct {
	Reason      string // why the VM stopped, e.g. "guest rebooted"
	Exits       int
	OutputBytes int64
	Truncated   bool
	Duration    time.Duration
}

// Machine is a configured Linux microVM. Create it with NewLinux, start
// it with Run, and always Close it.
type Machine struct {
	cfg     LinuxConfig
	sys     *kvm.System
	vm      *kvm.VM
	vcpu    *kvm.VCPU
	runner  *runner
	bus     *devices.PortBus
	console *devices.Serial
	out     *limitedWriter
	tail    *tailBuffer
	channel *Channel

	irqMu  sync.Mutex
	irqErr error
	reset  bool // guest requested a reboot (vCPU thread only)
	ran    bool
}

// NewLinux creates the VM, loads the kernel and wires up devices. Nothing
// runs until Run is called.
func NewLinux(cfg LinuxConfig) (m *Machine, err error) {
	if cfg.Cmdline == "" {
		cfg.Cmdline = DefaultCmdline
	}
	if cfg.MemSize == 0 {
		cfg.MemSize = 256 << 20
	}
	if cfg.MaxOutput == 0 {
		cfg.MaxOutput = 16 << 20
	}
	if cfg.Output == nil {
		cfg.Output = io.Discard
	}
	if cfg.MemSize%4096 != 0 {
		return nil, errors.New("memory size must be a multiple of 4 KiB")
	}

	m = &Machine{cfg: cfg}
	defer func() {
		if err != nil {
			m.Close()
		}
	}()

	if m.sys, err = kvm.Open(); err != nil {
		return nil, err
	}
	if m.vm, err = m.sys.CreateVM(); err != nil {
		return nil, err
	}
	// Order matters: the irqchip must exist before any vCPU.
	if err := m.vm.SetTSSAddr(tssAddr); err != nil {
		return nil, err
	}
	if err := m.vm.CreateIRQChip(); err != nil {
		return nil, err
	}
	if err := m.vm.CreatePIT2(); err != nil {
		return nil, err
	}

	mem, err := m.vm.AddMemory(0, 0, cfg.MemSize)
	if err != nil {
		return nil, err
	}
	entry, err := boot.Load(mem, boot.Linux{Kernel: cfg.Kernel, Initrd: cfg.Initrd, Cmdline: cfg.Cmdline})
	if err != nil {
		return nil, fmt.Errorf("load kernel: %w", err)
	}

	cpuid, err := m.sys.SupportedCPUID()
	if err != nil {
		return nil, err
	}
	if m.vcpu, err = m.vm.CreateVCPU(0); err != nil {
		return nil, err
	}
	if err := boot.SetupVCPU(m.vcpu, cpuid, entry); err != nil {
		return nil, fmt.Errorf("set up vcpu: %w", err)
	}
	m.runner = newRunner(m.vcpu)

	// Devices.
	m.tail = &tailBuffer{size: consoleTailSize}
	m.out = &limitedWriter{w: cfg.Output, remaining: cfg.MaxOutput}
	m.console = devices.NewSerial(io.MultiWriter(m.tail, m.out), m.irqLine(4))
	m.bus = &devices.PortBus{Trace: cfg.Trace}
	regs := []struct {
		base, size uint16
		dev        devices.PortDevice
	}{
		{0x3f8, 8, m.console},        // COM1: console, IRQ 4
		{0x70, 2, devices.NewCMOS()}, // RTC
		{0x60, 5, &devices.I8042{OnReset: func() { m.reset = true }}}, // 0x60-0x64
	}
	if cfg.AgentChannel {
		m.channel = newChannel()
		m.channel.serial = devices.NewSerial(guestWriter{m.channel}, m.irqLine(3))
		regs = append(regs, struct {
			base, size uint16
			dev        devices.PortDevice
		}{0x2f8, 8, m.channel.serial}) // COM2: agent channel, IRQ 3
	}
	for _, r := range regs {
		if err := m.bus.Register(r.base, r.size, r.dev); err != nil {
			return nil, err
		}
	}
	return m, nil
}

// irqLine returns a callback that drives a legacy interrupt line. It may
// be called from any goroutine (e.g. the agent channel's writer).
func (m *Machine) irqLine(irq uint32) func(bool) {
	return func(high bool) {
		if err := m.vm.IRQLine(irq, high); err != nil {
			m.irqMu.Lock()
			if m.irqErr == nil {
				m.irqErr = err
			}
			m.irqMu.Unlock()
			m.runner.Stop(err)
		}
	}
}

// Channel returns the agent channel, or nil if AgentChannel was not set.
// It reaches EOF once the VM stops.
func (m *Machine) Channel() *Channel { return m.channel }

// Stop asks a running VM to stop. Run then returns with Reason
// "stopped by host". Safe from any goroutine, before or during Run.
func (m *Machine) Stop() { m.runner.Stop(errStopped) }

// ConsoleTail returns the most recent console output, useful when a guest
// fails.
func (m *Machine) ConsoleTail() []byte { return m.tail.Bytes() }

// Run runs the guest on the calling goroutine until it reboots, shuts
// down, is stopped, or times out. It can only be called once.
func (m *Machine) Run() (LinuxResult, error) {
	var res LinuxResult
	if m.ran {
		return res, errors.New("machine already ran")
	}
	m.ran = true

	start := time.Now()
	exits, err := m.runner.run(m.cfg.Timeout, m.handleExit(&res))
	res.Exits = exits
	res.Duration = time.Since(start)
	res.OutputBytes, res.Truncated = m.out.written, m.out.truncated
	if errors.Is(err, errStopped) {
		res.Reason, err = "stopped by host", nil
	}
	if err == nil {
		err = m.console.Err()
	}
	if m.channel != nil {
		m.channel.close(io.EOF)
	}
	return res, err
}

func (m *Machine) handleExit(res *LinuxResult) exitHandler {
	return func(vcpu *kvm.VCPU) (bool, error) {
		switch vcpu.RunState().ExitReason {
		case kvm.ExitIO:
			d := vcpu.IO()
			data, err := vcpu.IOData()
			if err != nil {
				return true, err
			}
			if d.Direction == kvm.IODirOut {
				m.bus.Write(d.Port, data)
			} else {
				m.bus.Read(d.Port, data)
			}
			m.irqMu.Lock()
			irqErr := m.irqErr
			m.irqMu.Unlock()
			if irqErr != nil {
				return true, irqErr
			}
			if m.reset {
				res.Reason = "guest rebooted"
				return true, nil
			}
			return false, nil
		case kvm.ExitMMIO:
			// No MMIO devices yet: reads see all ones, writes are dropped.
			mm := vcpu.MMIO()
			if mm.IsWrite == 0 {
				for i := range mm.Data {
					mm.Data[i] = 0xff
				}
			}
			if m.cfg.Trace != nil {
				dir := "read"
				if mm.IsWrite != 0 {
					dir = "write"
				}
				fmt.Fprintf(m.cfg.Trace, "[ksar] unhandled mmio %s %#x len %d\n", dir, mm.PhysAddr, mm.Len)
			}
			return false, nil
		case kvm.ExitShutdown:
			// Triple fault: Linux's last-resort reboot method, or a crash.
			res.Reason = "guest shut down (triple fault)"
			return true, nil
		case kvm.ExitHLT:
			// With an in-kernel irqchip KVM handles HLT itself; seeing it
			// here means the guest halted for good.
			res.Reason = "guest halted"
			return true, nil
		}
		return true, fatalExit(vcpu)
	}
}

// Close destroys the VM and releases all its resources.
func (m *Machine) Close() error {
	if m.channel != nil {
		m.channel.close(io.EOF)
	}
	var errs []error
	if m.vm != nil {
		errs = append(errs, m.vm.Close())
	}
	if m.sys != nil {
		errs = append(errs, m.sys.Close())
	}
	m.vm, m.sys = nil, nil
	return errors.Join(errs...)
}

// RunLinux boots a Linux kernel in a fresh single-vCPU microVM and runs it
// until the guest reboots or shuts down, or the timeout expires. The VM is
// always destroyed before returning.
func RunLinux(cfg LinuxConfig) (LinuxResult, error) {
	m, err := NewLinux(cfg)
	if err != nil {
		return LinuxResult{}, err
	}
	defer m.Close()
	return m.Run()
}
