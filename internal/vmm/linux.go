package vmm

import (
	"errors"
	"fmt"
	"io"
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
//   - 8250.nr_uarts=1: don't probe COM2-4
const DefaultCmdline = "console=ttyS0 earlyprintk=serial,ttyS0,115200 reboot=k panic=-1 pci=off acpi=off 8250.nr_uarts=1"

// tssAddr is the 3-page region Intel VMX needs, placed just under the
// 4 GiB boundary, far from guest RAM (capped at 3 GiB).
const tssAddr = 0xfffbd000

// LinuxConfig describes a Linux microVM.
type LinuxConfig struct {
	Kernel    []byte // bzImage
	Initrd    []byte // optional initramfs
	Cmdline   string // defaults to DefaultCmdline
	MemSize   int    // guest RAM in bytes (default 256 MiB)
	Timeout   time.Duration
	Output    io.Writer // serial console output (may be nil)
	MaxOutput int64     // cap on console bytes (default 16 MiB)
	Trace     io.Writer // if set, unhandled port/MMIO accesses are logged
}

// LinuxResult reports how a Linux guest run ended.
type LinuxResult struct {
	Reason      string // why the VM stopped, e.g. "guest rebooted"
	Exits       int
	OutputBytes int64
	Truncated   bool
	Duration    time.Duration
}

// RunLinux boots a Linux kernel in a fresh single-vCPU microVM and runs it
// until the guest reboots or shuts down, or the timeout expires. The VM is
// always destroyed before returning.
func RunLinux(cfg LinuxConfig) (LinuxResult, error) {
	var res LinuxResult
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
		return res, errors.New("memory size must be a multiple of 4 KiB")
	}

	sys, err := kvm.Open()
	if err != nil {
		return res, err
	}
	defer sys.Close()

	vm, err := sys.CreateVM()
	if err != nil {
		return res, err
	}
	defer vm.Close()

	// Order matters: the irqchip must exist before any vCPU.
	if err := vm.SetTSSAddr(tssAddr); err != nil {
		return res, err
	}
	if err := vm.CreateIRQChip(); err != nil {
		return res, err
	}
	if err := vm.CreatePIT2(); err != nil {
		return res, err
	}

	mem, err := vm.AddMemory(0, 0, cfg.MemSize)
	if err != nil {
		return res, err
	}
	entry, err := boot.Load(mem, boot.Linux{Kernel: cfg.Kernel, Initrd: cfg.Initrd, Cmdline: cfg.Cmdline})
	if err != nil {
		return res, fmt.Errorf("load kernel: %w", err)
	}

	cpuid, err := sys.SupportedCPUID()
	if err != nil {
		return res, err
	}
	vcpu, err := vm.CreateVCPU(0)
	if err != nil {
		return res, err
	}
	if err := boot.SetupVCPU(vcpu, cpuid, entry); err != nil {
		return res, fmt.Errorf("set up vcpu: %w", err)
	}

	// Devices.
	out := &limitedWriter{w: cfg.Output, remaining: cfg.MaxOutput}
	var irqErr error
	serial := devices.NewSerial(out, func(high bool) {
		if err := vm.IRQLine(4, high); err != nil && irqErr == nil {
			irqErr = err
		}
	})
	resetRequested := false
	bus := &devices.PortBus{Trace: cfg.Trace}
	for _, d := range []struct {
		base, size uint16
		dev        devices.PortDevice
	}{
		{0x3f8, 8, serial},           // COM1, IRQ 4
		{0x70, 2, devices.NewCMOS()}, // RTC
		{0x60, 5, &devices.I8042{OnReset: func() { resetRequested = true }}}, // 0x60-0x64
	} {
		if err := bus.Register(d.base, d.size, d.dev); err != nil {
			return res, err
		}
	}

	start := time.Now()
	res.Exits, err = runVCPU(vcpu, cfg.Timeout, func(vcpu *kvm.VCPU) (bool, error) {
		switch vcpu.RunState().ExitReason {
		case kvm.ExitIO:
			d := vcpu.IO()
			data, err := vcpu.IOData()
			if err != nil {
				return true, err
			}
			if d.Direction == kvm.IODirOut {
				bus.Write(d.Port, data)
			} else {
				bus.Read(d.Port, data)
			}
			if irqErr != nil {
				return true, irqErr
			}
			if resetRequested {
				res.Reason = "guest rebooted"
				return true, nil
			}
			return false, nil
		case kvm.ExitMMIO:
			// No MMIO devices yet: reads see all ones, writes are dropped.
			m := vcpu.MMIO()
			if m.IsWrite == 0 {
				for i := range m.Data {
					m.Data[i] = 0xff
				}
			}
			if cfg.Trace != nil {
				fmt.Fprintf(cfg.Trace, "[ksar] unhandled mmio %s %#x len %d\n",
					map[bool]string{true: "write", false: "read"}[m.IsWrite != 0], m.PhysAddr, m.Len)
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
	})
	res.Duration = time.Since(start)
	res.OutputBytes, res.Truncated = out.written, out.truncated
	if err == nil {
		err = serial.Err()
	}
	return res, err
}
