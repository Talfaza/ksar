package kvm

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"
)

func iowr(nr, size uintptr) uintptr { return ioc(iocRead|iocWrite, kvmio, nr, size) }

var (
	kvmGetSupportedCPUID = iowr(0x05, 8) // struct kvm_cpuid2 header
	kvmSetTSSAddr        = io(0x47)
	kvmCreateIRQChip     = io(0x60)
	kvmIRQLine           = iow(0x61, 8)  // struct kvm_irq_level
	kvmCreatePIT2        = iow(0x77, 64) // struct kvm_pit_config
	kvmSetFPU            = iow(0x8d, 416)
	kvmGetLAPIC          = ior(0x8e, 1024)
	kvmSetLAPIC          = iow(0x8f, 1024)
	kvmSetCPUID2         = iow(0x90, 8)
	kvmSetMSRs           = iow(0x89, 8) // struct kvm_msrs header
)

// MSR is one model-specific register value (struct kvm_msr_entry).
type MSR struct {
	Index uint32
	_     uint32
	Data  uint64
}

// SetMSRs writes model-specific registers.
func (c *VCPU) SetMSRs(msrs []MSR) error {
	// struct kvm_msrs { __u32 nmsrs; __u32 pad; struct kvm_msr_entry entries[]; }
	buf := make([]uint64, 1+2*len(msrs))
	*(*uint32)(unsafe.Pointer(&buf[0])) = uint32(len(msrs))
	copy(unsafe.Slice((*MSR)(unsafe.Pointer(&buf[1])), len(msrs)), msrs)
	n, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(c.fd), kvmSetMSRs, uintptr(unsafe.Pointer(&buf[0])))
	if errno != 0 {
		return wrap("KVM_SET_MSRS", errno)
	}
	if int(n) != len(msrs) {
		return fmt.Errorf("KVM_SET_MSRS: only %d of %d MSRs accepted (failed at %#x)",
			n, len(msrs), msrs[n].Index)
	}
	return nil
}

// ExitMMIODetails is the "mmio" member of the kvm_run union (KVM_EXIT_MMIO).
type ExitMMIODetails struct {
	PhysAddr uint64
	Data     [8]uint8
	Len      uint32
	IsWrite  uint8
	_        [3]uint8
}

// MMIO returns the details of a KVM_EXIT_MMIO exit.
func (c *VCPU) MMIO() *ExitMMIODetails {
	return (*ExitMMIODetails)(unsafe.Pointer(&c.run[runUnionOffset]))
}

// CPUIDEntry is struct kvm_cpuid_entry2.
type CPUIDEntry struct {
	Function uint32
	Index    uint32
	Flags    uint32
	EAX      uint32
	EBX      uint32
	ECX      uint32
	EDX      uint32
	_        [3]uint32
}

// FPU is struct kvm_fpu.
type FPU struct {
	FPR        [8][16]uint8
	FCW        uint16
	FSW        uint16
	FTWX       uint8
	_          uint8
	LastOpcode uint16
	LastIP     uint64
	LastDP     uint64
	XMM        [16][16]uint8
	MXCSR      uint32
	_          uint32
}

// LAPICState is struct kvm_lapic_state: the raw local APIC register page.
type LAPICState struct {
	Regs [1024]byte
}

// SupportedCPUID returns the CPUID leaves KVM can expose to guests on this host.
func (s *System) SupportedCPUID() ([]CPUIDEntry, error) {
	for n := 128; n <= 4096; n *= 2 {
		entries, err := getCPUID(s.fd, n)
		if errors.Is(err, syscall.E2BIG) {
			continue
		}
		return entries, err
	}
	return nil, errors.New("KVM_GET_SUPPORTED_CPUID: too many entries")
}

func getCPUID(fd, n int) ([]CPUIDEntry, error) {
	// struct kvm_cpuid2 { __u32 nent; __u32 padding; entries[]; }
	// Backed by []uint64 so the buffer is 8-byte aligned.
	entrySize := int(unsafe.Sizeof(CPUIDEntry{}))
	buf := make([]uint64, (8+n*entrySize+7)/8)
	*(*uint32)(unsafe.Pointer(&buf[0])) = uint32(n)
	if err := ioctlPtr(fd, kvmGetSupportedCPUID, unsafe.Pointer(&buf[0])); err != nil {
		return nil, err
	}
	nent := int(*(*uint32)(unsafe.Pointer(&buf[0])))
	if nent > n {
		return nil, fmt.Errorf("kernel returned %d cpuid entries, buffer holds %d", nent, n)
	}
	src := unsafe.Slice((*CPUIDEntry)(unsafe.Add(unsafe.Pointer(&buf[0]), 8)), nent)
	return append([]CPUIDEntry(nil), src...), nil
}

// SetCPUID sets the CPUID leaves the guest will see.
func (c *VCPU) SetCPUID(entries []CPUIDEntry) error {
	entrySize := int(unsafe.Sizeof(CPUIDEntry{}))
	buf := make([]uint64, (8+len(entries)*entrySize+7)/8)
	*(*uint32)(unsafe.Pointer(&buf[0])) = uint32(len(entries))
	dst := unsafe.Slice((*CPUIDEntry)(unsafe.Add(unsafe.Pointer(&buf[0]), 8)), len(entries))
	copy(dst, entries)
	return wrap("KVM_SET_CPUID2", ioctlPtr(c.fd, kvmSetCPUID2, unsafe.Pointer(&buf[0])))
}

// SetTSSAddr reserves 3 pages of guest physical address space that Intel
// VMX needs internally. It must not overlap guest RAM or devices.
func (vm *VM) SetTSSAddr(addr uint64) error {
	_, err := ioctl(vm.fd, kvmSetTSSAddr, uintptr(addr))
	return wrap("KVM_SET_TSS_ADDR", err)
}

// CreateIRQChip creates the in-kernel PIC, IOAPIC and per-vCPU local APICs.
// Must be called before creating vCPUs.
func (vm *VM) CreateIRQChip() error {
	_, err := ioctl(vm.fd, kvmCreateIRQChip, 0)
	return wrap("KVM_CREATE_IRQCHIP", err)
}

// CreatePIT2 creates the in-kernel 8254 timer (ports 0x40-0x43 and 0x61).
func (vm *VM) CreatePIT2() error {
	var cfg struct {
		Flags uint32
		_     [15]uint32
	}
	cfg.Flags = 1 // KVM_PIT_SPEAKER_DUMMY: emulate port 0x61 in-kernel too
	return wrap("KVM_CREATE_PIT2", ioctlPtr(vm.fd, kvmCreatePIT2, unsafe.Pointer(&cfg)))
}

// IRQLine sets the level of a legacy interrupt line on the in-kernel
// interrupt controllers. Safe to call from any thread.
func (vm *VM) IRQLine(irq uint32, high bool) error {
	lvl := struct{ IRQ, Level uint32 }{IRQ: irq}
	if high {
		lvl.Level = 1
	}
	return wrap("KVM_IRQ_LINE", ioctlPtr(vm.fd, kvmIRQLine, unsafe.Pointer(&lvl)))
}

// SetFPU writes the FPU/SSE state.
func (c *VCPU) SetFPU(f FPU) error {
	return wrap("KVM_SET_FPU", ioctlPtr(c.fd, kvmSetFPU, unsafe.Pointer(&f)))
}

// GetLAPIC reads the local APIC register page.
func (c *VCPU) GetLAPIC() (LAPICState, error) {
	var l LAPICState
	err := ioctlPtr(c.fd, kvmGetLAPIC, unsafe.Pointer(&l))
	return l, wrap("KVM_GET_LAPIC", err)
}

// SetLAPIC writes the local APIC register page.
func (c *VCPU) SetLAPIC(l LAPICState) error {
	return wrap("KVM_SET_LAPIC", ioctlPtr(c.fd, kvmSetLAPIC, unsafe.Pointer(&l)))
}
