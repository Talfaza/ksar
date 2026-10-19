// Package kvm is a thin, dependency-free wrapper around the Linux /dev/kvm API.
//
// The structs here mirror the C layouts in <linux/kvm.h> for x86_64 exactly.
// types_test.go checks their sizes, because a wrong size or padding silently
// corrupts what the kernel reads and writes.
package kvm

// ioctl request encoding, from <asm-generic/ioctl.h>.
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2

	kvmio = 0xAE
)

func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | typ<<8 | nr
}

func io(nr uintptr) uintptr        { return ioc(iocNone, kvmio, nr, 0) }
func ior(nr, size uintptr) uintptr { return ioc(iocRead, kvmio, nr, size) }
func iow(nr, size uintptr) uintptr { return ioc(iocWrite, kvmio, nr, size) }

// ioctl request numbers.
var (
	kvmGetAPIVersion       = io(0x00)
	kvmCreateVM            = io(0x01)
	kvmGetVCPUMmapSize     = io(0x04)
	kvmCreateVCPU          = io(0x41)
	kvmSetUserMemoryRegion = iow(0x46, 32) // struct kvm_userspace_memory_region
	kvmRun                 = io(0x80)
	kvmGetRegs             = ior(0x81, 144) // struct kvm_regs
	kvmSetRegs             = iow(0x82, 144)
	kvmGetSregs            = ior(0x83, 312) // struct kvm_sregs
	kvmSetSregs            = iow(0x84, 312)
)

// APIVersion is the only KVM API version that has ever been stable.
const APIVersion = 12

// Exit reasons reported in Run.ExitReason.
const (
	ExitUnknown       = 0
	ExitException     = 1
	ExitIO            = 2
	ExitHypercall     = 3
	ExitDebug         = 4
	ExitHLT           = 5
	ExitMMIO          = 6
	ExitShutdown      = 8
	ExitFailEntry     = 9
	ExitIntr          = 10
	ExitInternalError = 17
	ExitSystemEvent   = 24
)

// Directions for ExitIO.
const (
	IODirIn  = 0
	IODirOut = 1
)

// userspaceMemoryRegion is struct kvm_userspace_memory_region.
type userspaceMemoryRegion struct {
	Slot          uint32
	Flags         uint32
	GuestPhysAddr uint64
	MemorySize    uint64
	UserspaceAddr uint64
}

// Regs is struct kvm_regs: the general-purpose registers.
type Regs struct {
	RAX, RBX, RCX, RDX uint64
	RSI, RDI, RSP, RBP uint64
	R8, R9, R10, R11   uint64
	R12, R13, R14, R15 uint64
	RIP, RFLAGS        uint64
}

// Segment is struct kvm_segment.
type Segment struct {
	Base     uint64
	Limit    uint32
	Selector uint16
	Type     uint8
	Present  uint8
	DPL      uint8
	DB       uint8
	S        uint8
	L        uint8
	G        uint8
	AVL      uint8
	Unusable uint8
	_        uint8
}

// DTable is struct kvm_dtable (GDT/IDT descriptor).
type DTable struct {
	Base  uint64
	Limit uint16
	_     [3]uint16
}

// Sregs is struct kvm_sregs: segment and control registers.
type Sregs struct {
	CS, DS, ES, FS, GS, SS Segment
	TR, LDT                Segment
	GDT, IDT               DTable
	CR0, CR2, CR3, CR4     uint64
	CR8                    uint64
	EFER                   uint64
	APICBase               uint64
	InterruptBitmap        [4]uint64
}

// Run is the fixed header of struct kvm_run, the page shared between the
// kernel and the VMM for each vCPU. The exit-specific union starts at
// runUnionOffset.
type Run struct {
	RequestInterruptWindow     uint8
	ImmediateExit              uint8
	_                          [6]uint8
	ExitReason                 uint32
	ReadyForInterruptInjection uint8
	IFFlag                     uint8
	Flags                      uint16
	CR8                        uint64
	APICBase                   uint64
}

const runUnionOffset = 32

// ExitIODetails is the "io" member of the kvm_run union (KVM_EXIT_IO).
// The data lives inside the kvm_run mapping at DataOffset.
type ExitIODetails struct {
	Direction  uint8
	Size       uint8
	Port       uint16
	Count      uint32
	DataOffset uint64
}

// ExitFailEntryDetails is the "fail_entry" member (KVM_EXIT_FAIL_ENTRY).
type ExitFailEntryDetails struct {
	HardwareEntryFailureReason uint64
	CPU                        uint32
}

// ExitInternalErrorDetails is the "internal" member (KVM_EXIT_INTERNAL_ERROR).
type ExitInternalErrorDetails struct {
	Suberror uint32
	NData    uint32
	Data     [16]uint64
}
