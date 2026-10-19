package kvm

import (
	"testing"
	"unsafe"
)

// The kernel reads and writes these structs byte for byte, so their Go
// layout must match <linux/kvm.h> on x86_64 exactly.
func TestStructSizes(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"kvm_userspace_memory_region", unsafe.Sizeof(userspaceMemoryRegion{}), 32},
		{"kvm_regs", unsafe.Sizeof(Regs{}), 144},
		{"kvm_segment", unsafe.Sizeof(Segment{}), 24},
		{"kvm_dtable", unsafe.Sizeof(DTable{}), 16},
		{"kvm_sregs", unsafe.Sizeof(Sregs{}), 312},
		{"kvm_run header", unsafe.Sizeof(Run{}), runUnionOffset},
		{"kvm_run.io", unsafe.Sizeof(ExitIODetails{}), 16},
		{"kvm_run.mmio", unsafe.Sizeof(ExitMMIODetails{}), 24},
		{"kvm_cpuid_entry2", unsafe.Sizeof(CPUIDEntry{}), 40},
		{"kvm_fpu", unsafe.Sizeof(FPU{}), 416},
		{"kvm_lapic_state", unsafe.Sizeof(LAPICState{}), 1024},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("sizeof(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}
	if off := unsafe.Offsetof(Run{}.ExitReason); off != 8 {
		t.Errorf("kvm_run.exit_reason offset = %d, want 8", off)
	}
	if off := unsafe.Offsetof(Sregs{}.CR0); off != 224 {
		t.Errorf("kvm_sregs.cr0 offset = %d, want 224", off)
	}
}

// Values taken from the kernel headers (see e.g. strace output).
func TestIoctlNumbers(t *testing.T) {
	cases := []struct {
		name      string
		got, want uintptr
	}{
		{"KVM_GET_API_VERSION", kvmGetAPIVersion, 0xAE00},
		{"KVM_CREATE_VM", kvmCreateVM, 0xAE01},
		{"KVM_GET_VCPU_MMAP_SIZE", kvmGetVCPUMmapSize, 0xAE04},
		{"KVM_CREATE_VCPU", kvmCreateVCPU, 0xAE41},
		{"KVM_SET_USER_MEMORY_REGION", kvmSetUserMemoryRegion, 0x4020AE46},
		{"KVM_RUN", kvmRun, 0xAE80},
		{"KVM_GET_REGS", kvmGetRegs, 0x8090AE81},
		{"KVM_SET_REGS", kvmSetRegs, 0x4090AE82},
		{"KVM_GET_SREGS", kvmGetSregs, 0x8138AE83},
		{"KVM_SET_SREGS", kvmSetSregs, 0x4138AE84},
		{"KVM_GET_SUPPORTED_CPUID", kvmGetSupportedCPUID, 0xC008AE05},
		{"KVM_SET_TSS_ADDR", kvmSetTSSAddr, 0xAE47},
		{"KVM_CREATE_IRQCHIP", kvmCreateIRQChip, 0xAE60},
		{"KVM_IRQ_LINE", kvmIRQLine, 0x4008AE61},
		{"KVM_CREATE_PIT2", kvmCreatePIT2, 0x4040AE77},
		{"KVM_SET_FPU", kvmSetFPU, 0x41A0AE8D},
		{"KVM_GET_LAPIC", kvmGetLAPIC, 0x8400AE8E},
		{"KVM_SET_LAPIC", kvmSetLAPIC, 0x4400AE8F},
		{"KVM_SET_CPUID2", kvmSetCPUID2, 0x4008AE90},
		{"KVM_SET_MSRS", kvmSetMSRs, 0x4008AE89},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}
