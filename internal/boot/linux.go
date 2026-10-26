// Package boot loads a Linux bzImage into guest memory following the x86
// boot protocol (Documentation/arch/x86/boot.rst) and prepares a vCPU to
// enter the kernel's 64-bit entry point directly, skipping real mode.
package boot

import (
	"encoding/binary"
	"errors"
	"fmt"

	"ksar/internal/kvm"
)

// Guest physical memory layout used during boot. Everything below 1 MiB
// except what's listed here is left free for the kernel.
const (
	gdtAddr      = 0x500
	idtAddr      = 0x520
	zeroPageAddr = 0x7000 // struct boot_params
	stackTop     = 0x8ff0 // initial RSP; the kernel switches stacks quickly
	pml4Addr     = 0x9000
	pdptAddr     = 0xa000
	pdAddr       = 0xb000 // 4 page directories: 0xb000-0xefff, identity map 4 GiB
	cmdlineAddr  = 0x20000
	ebdaStart    = 0x9fc00  // end of low RAM reported to the guest
	KernelAddr   = 0x100000 // protected-mode kernel load address (1 MiB)

	// MaxMemory keeps all RAM below the 32-bit PCI/APIC hole at 3 GiB.
	MaxMemory = 3 << 30
)

// Setup header offsets inside the bzImage and inside struct boot_params
// (the header sits at the same offset in both).
const (
	hdrSetupSects    = 0x1f1
	hdrJumpLen       = 0x201
	hdrMagic         = 0x202 // "HdrS"
	hdrVersion       = 0x206
	hdrTypeOfLoader  = 0x210
	hdrLoadFlags     = 0x211
	hdrRamdiskImage  = 0x218
	hdrRamdiskSize   = 0x21c
	hdrCmdLinePtr    = 0x228
	hdrInitrdAddrMax = 0x22c
	hdrXLoadFlags    = 0x236
	hdrCmdlineSize   = 0x238
	hdrInitSize      = 0x260
	bpE820Entries    = 0x1e8
	bpE820Table      = 0x2d0
	maxE820Entries   = 128
	loadedHigh       = 0x01 // loadflags: protected-mode code at 1 MiB
	canUseHeap       = 0x80
	xlfKernel64      = 0x01 // xloadflags: has 64-bit entry at +0x200
	minVersion       = 0x020c
	e820RAM          = 1
	entry64Offset    = 0x200
	defaultInitrdMax = 0x37ffffff
	kernelDecompBase = 16 << 20 // LOAD_PHYSICAL_ADDR on x86_64
)

// Linux describes what to boot.
type Linux struct {
	Kernel  []byte // bzImage
	Initrd  []byte // optional initramfs (cpio, optionally compressed)
	Cmdline string

	// Framebuffer, if set, is announced to the kernel as a firmware
	// linear framebuffer (32-bit xRGB), which simpledrm then drives.
	Framebuffer *Framebuffer
}

// Framebuffer is a linear framebuffer at a guest physical address outside
// RAM, with 4 bytes per pixel: blue, green, red, unused.
type Framebuffer struct {
	Base          uint64
	Width, Height int
	Stride        int // bytes per line
}

// screen_info (offset 0 of boot_params) fields for a VESA linear framebuffer.
const (
	siIsVGA        = 0x0f
	siLfbWidth     = 0x12
	siLfbHeight    = 0x14
	siLfbDepth     = 0x16
	siLfbBase      = 0x18
	siLfbSize      = 0x1c // in 64 KiB units for VLFB
	siLfbLineLen   = 0x24
	siRedSize      = 0x26 // then red_pos, green size/pos, blue, rsvd
	siCapabilities = 0x36
	siExtLfbBase   = 0x3a
	videoTypeVLFB  = 0x23
	video64BitBase = 1 << 1
	e820Reserved   = 2
)

// Load copies the kernel, initrd and command line into mem (guest RAM
// starting at physical address 0) and builds boot_params. It returns the
// 64-bit entry point.
func Load(mem []byte, l Linux) (entry uint64, err error) {
	k := l.Kernel
	if len(k) < 0x1000 {
		return 0, errors.New("kernel image too small to be a bzImage")
	}
	if string(k[hdrMagic:hdrMagic+4]) != "HdrS" {
		return 0, errors.New("not a bzImage (missing HdrS magic); pass a bzImage such as /boot/vmlinuz-*")
	}
	version := binary.LittleEndian.Uint16(k[hdrVersion:])
	if version < minVersion {
		return 0, fmt.Errorf("boot protocol %#x too old, need %#x", version, minVersion)
	}
	if k[hdrLoadFlags]&loadedHigh == 0 {
		return 0, errors.New("kernel is not a bzImage (LOADED_HIGH not set)")
	}
	if binary.LittleEndian.Uint16(k[hdrXLoadFlags:])&xlfKernel64 == 0 {
		return 0, errors.New("kernel has no 64-bit entry point (XLF_KERNEL_64)")
	}
	memSize := uint64(len(mem))
	if memSize > MaxMemory {
		return 0, fmt.Errorf("guest memory %d MiB exceeds %d MiB", memSize>>20, MaxMemory>>20)
	}

	setupSects := int(k[hdrSetupSects])
	if setupSects == 0 {
		setupSects = 4
	}
	pmOffset := (setupSects + 1) * 512
	if pmOffset >= len(k) {
		return 0, errors.New("truncated kernel image")
	}
	pmKernel := k[pmOffset:]

	// The decompressor relocates itself out of the way and unpacks the
	// kernel at 16 MiB, needing init_size bytes there. Overlap between the
	// compressed image at 1 MiB and that area is expected and handled by
	// the kernel. Make sure all of it, plus the initrd, fits in RAM.
	initSize := uint64(binary.LittleEndian.Uint32(k[hdrInitSize:]))
	if need := kernelDecompBase + initSize + uint64(len(l.Initrd)) + 16<<20; need > memSize {
		return 0, fmt.Errorf("not enough guest memory: need at least %d MiB", need>>20+1)
	}
	copy(mem[KernelAddr:], pmKernel)

	// boot_params ("zero page"): zeroed, then the setup header from the image.
	zp := mem[zeroPageAddr : zeroPageAddr+4096]
	clear(zp)
	hdrEnd := hdrMagic + int(k[hdrJumpLen])
	if hdrEnd <= hdrMagic || hdrEnd > 0x400 {
		return 0, errors.New("invalid setup header length")
	}
	copy(zp[hdrSetupSects:], k[hdrSetupSects:hdrEnd])
	zp[hdrTypeOfLoader] = 0xff // "undefined" boot loader
	zp[hdrLoadFlags] |= canUseHeap

	// Command line.
	maxCmd := binary.LittleEndian.Uint32(k[hdrCmdlineSize:])
	if maxCmd == 0 {
		maxCmd = 255
	}
	if uint32(len(l.Cmdline)) > maxCmd {
		return 0, fmt.Errorf("kernel command line longer than %d bytes", maxCmd)
	}
	cmd := mem[cmdlineAddr : cmdlineAddr+int(maxCmd)+1]
	clear(cmd)
	copy(cmd, l.Cmdline)
	binary.LittleEndian.PutUint32(zp[hdrCmdLinePtr:], cmdlineAddr)

	// Initrd: as high as allowed, page aligned.
	if len(l.Initrd) > 0 {
		maxAddr := uint64(binary.LittleEndian.Uint32(k[hdrInitrdAddrMax:]))
		if maxAddr == 0 {
			maxAddr = defaultInitrdMax
		}
		top := min(memSize, maxAddr+1)
		addr := (top - uint64(len(l.Initrd))) &^ 0xfff
		if addr < kernelDecompBase+initSize {
			return 0, errors.New("initrd overlaps the kernel; increase guest memory")
		}
		copy(mem[addr:], l.Initrd)
		binary.LittleEndian.PutUint32(zp[hdrRamdiskImage:], uint32(addr))
		binary.LittleEndian.PutUint32(zp[hdrRamdiskSize:], uint32(len(l.Initrd)))
	}

	// e820 memory map: low RAM up to the EBDA, then everything from 1 MiB.
	// The gap 0x9fc00-0x100000 (VGA, BIOS ROM area) is left unreported.
	e820 := []struct{ addr, size uint64 }{
		{0, ebdaStart},
		{KernelAddr, memSize - KernelAddr},
	}
	for i, e := range e820 {
		off := bpE820Table + i*20
		binary.LittleEndian.PutUint64(zp[off:], e.addr)
		binary.LittleEndian.PutUint64(zp[off+8:], e.size)
		binary.LittleEndian.PutUint32(zp[off+16:], e820RAM)
	}
	zp[bpE820Entries] = byte(len(e820))

	if fb := l.Framebuffer; fb != nil {
		if err := writeScreenInfo(zp, fb, memSize); err != nil {
			return 0, err
		}
		// Mark the framebuffer reserved so nothing treats it as RAM.
		off := bpE820Table + len(e820)*20
		size := uint64(fb.Stride * fb.Height)
		binary.LittleEndian.PutUint64(zp[off:], fb.Base)
		binary.LittleEndian.PutUint64(zp[off+8:], (size+0xffff)&^0xffff)
		binary.LittleEndian.PutUint32(zp[off+16:], e820Reserved)
		zp[bpE820Entries]++
	}

	writePageTables(mem)
	writeGDT(mem)
	return KernelAddr + entry64Offset, nil
}

func writeScreenInfo(zp []byte, fb *Framebuffer, memSize uint64) error {
	size := uint64(fb.Stride) * uint64(fb.Height)
	if fb.Width <= 0 || fb.Height <= 0 || fb.Width > 0xffff || fb.Height > 0xffff ||
		fb.Stride < fb.Width*4 || fb.Stride > 0xffff || fb.Base%4096 != 0 || fb.Base < memSize {
		return fmt.Errorf("invalid framebuffer %dx%d stride %d at %#x", fb.Width, fb.Height, fb.Stride, fb.Base)
	}
	le := binary.LittleEndian
	zp[siIsVGA] = videoTypeVLFB
	le.PutUint16(zp[siLfbWidth:], uint16(fb.Width))
	le.PutUint16(zp[siLfbHeight:], uint16(fb.Height))
	le.PutUint16(zp[siLfbDepth:], 32)
	le.PutUint32(zp[siLfbBase:], uint32(fb.Base))
	le.PutUint32(zp[siLfbSize:], uint32((size+0xffff)>>16))
	le.PutUint16(zp[siLfbLineLen:], uint16(fb.Stride))
	copy(zp[siRedSize:], []byte{8, 16, 8, 8, 8, 0, 8, 24}) // xRGB8888
	if fb.Base>>32 != 0 {
		le.PutUint32(zp[siCapabilities:], video64BitBase)
		le.PutUint32(zp[siExtLfbBase:], uint32(fb.Base>>32))
	}
	return nil
}

// writePageTables identity-maps the first 4 GiB with 2 MiB pages, which
// covers the kernel, boot_params, the command line and the initrd.
func writePageTables(mem []byte) {
	const present, writable, huge = 1, 2, 0x80
	clear(mem[pml4Addr : pdAddr+4*0x1000])
	binary.LittleEndian.PutUint64(mem[pml4Addr:], pdptAddr|present|writable)
	for i := range uint64(4) {
		pd := pdAddr + i*0x1000
		binary.LittleEndian.PutUint64(mem[pdptAddr+i*8:], pd|present|writable)
		for j := range uint64(512) {
			phys := i<<30 | j<<21
			binary.LittleEndian.PutUint64(mem[pd+j*8:], phys|present|writable|huge)
		}
	}
}

// GDT selectors.
const (
	selCode = 0x08
	selData = 0x10
	selTSS  = 0x18
)

// Segment descriptors matching the GDT below.
var (
	codeSeg = kvm.Segment{Base: 0, Limit: 0xffffffff, Selector: selCode,
		Type: 0xb, Present: 1, S: 1, L: 1, G: 1} // 64-bit code, exec/read, accessed
	dataSeg = kvm.Segment{Base: 0, Limit: 0xffffffff, Selector: selData,
		Type: 0x3, Present: 1, S: 1, DB: 1, G: 1} // data, read/write, accessed
	tssSeg = kvm.Segment{Base: 0, Limit: 0xffffffff, Selector: selTSS,
		Type: 0xb, Present: 1, G: 1} // busy 64-bit TSS
)

func writeGDT(mem []byte) {
	gdt := []uint64{
		0,                  // null
		0x00af9b000000ffff, // code: G=1 L=1 P=1 DPL=0 type=0xb limit=0xfffff
		0x00cf93000000ffff, // data: G=1 D/B=1 P=1 DPL=0 type=0x3
		0x008f8b000000ffff, // TSS:  G=1 P=1 type=0xb
	}
	for i, e := range gdt {
		binary.LittleEndian.PutUint64(mem[gdtAddr+i*8:], e)
	}
	binary.LittleEndian.PutUint64(mem[idtAddr:], 0)
}

// SetupVCPU puts the vCPU in 64-bit long mode with paging on, pointing at
// entry with RSI holding the boot_params address, as the 64-bit boot
// protocol requires.
func SetupVCPU(vcpu *kvm.VCPU, cpuid []kvm.CPUIDEntry, entry uint64) error {
	if err := vcpu.SetCPUID(guestCPUID(cpuid, vcpu.ID)); err != nil {
		return err
	}

	s, err := vcpu.GetSregs()
	if err != nil {
		return err
	}
	const (
		cr0PE, cr0ET, cr0NW, cr0CD, cr0PG = 1, 1 << 4, 1 << 29, 1 << 30, 1 << 31
		cr4PAE                            = 1 << 5
		eferLME, eferLMA                  = 1 << 8, 1 << 10
	)
	s.GDT = kvm.DTable{Base: gdtAddr, Limit: 4*8 - 1}
	s.IDT = kvm.DTable{Base: idtAddr, Limit: 7}
	s.CS = codeSeg
	s.DS, s.ES, s.FS, s.GS, s.SS = dataSeg, dataSeg, dataSeg, dataSeg, dataSeg
	s.TR = tssSeg
	s.CR3 = pml4Addr
	s.CR4 |= cr4PAE
	s.CR0 = (s.CR0 &^ (cr0CD | cr0NW)) | cr0PE | cr0ET | cr0PG
	s.EFER |= eferLME | eferLMA
	if err := vcpu.SetSregs(s); err != nil {
		return err
	}

	if err := vcpu.SetRegs(kvm.Regs{
		RIP:    entry,
		RSI:    zeroPageAddr,
		RSP:    stackTop,
		RBP:    stackTop,
		RFLAGS: 0x2,
	}); err != nil {
		return err
	}

	// IA32_MISC_ENABLE.FAST_STRING: firmware normally turns on fast
	// "rep movs/stos"; without it Linux disables them and copies slower.
	const msrMiscEnable, fastStrings = 0x1a0, 1
	if err := vcpu.SetMSRs([]kvm.MSR{{Index: msrMiscEnable, Data: fastStrings}}); err != nil {
		return err
	}

	// Sane FPU defaults: x87 control word and SSE MXCSR reset values.
	if err := vcpu.SetFPU(kvm.FPU{FCW: 0x37f, MXCSR: 0x1f80}); err != nil {
		return err
	}

	// Local APIC: route the legacy PIC through LINT0 (ExtINT) and NMIs
	// through LINT1, like a PC's "virtual wire" mode.
	lapic, err := vcpu.GetLAPIC()
	if err != nil {
		return err
	}
	const lvtLINT0, lvtLINT1, modeExtINT, modeNMI = 0x350, 0x360, 7, 4
	for _, r := range []struct{ off, mode uint32 }{{lvtLINT0, modeExtINT}, {lvtLINT1, modeNMI}} {
		v := binary.LittleEndian.Uint32(lapic.Regs[r.off:])
		v = v&^0x700 | r.mode<<8
		binary.LittleEndian.PutUint32(lapic.Regs[r.off:], v)
	}
	return vcpu.SetLAPIC(lapic)
}

// guestCPUID adjusts the host-supported CPUID table for one vCPU.
func guestCPUID(supported []kvm.CPUIDEntry, vcpuID int) []kvm.CPUIDEntry {
	out := append([]kvm.CPUIDEntry(nil), supported...)
	for i := range out {
		e := &out[i]
		switch e.Function {
		case 1:
			e.EBX = e.EBX&0x00ffffff | uint32(vcpuID)<<24 // initial APIC ID
			e.ECX |= 1 << 31                              // "running under a hypervisor"
		case 0xb, 0x1f:
			e.EDX = uint32(vcpuID) // x2APIC ID
		case 0x80000002, 0x80000003, 0x80000004:
			// KVM leaves the brand string empty. Use a fixed name rather
			// than the host's, so guests learn less about the host.
			b := brandString[(e.Function-0x80000002)*16:]
			e.EAX = binary.LittleEndian.Uint32(b[0:])
			e.EBX = binary.LittleEndian.Uint32(b[4:])
			e.ECX = binary.LittleEndian.Uint32(b[8:])
			e.EDX = binary.LittleEndian.Uint32(b[12:])
		}
	}
	return out
}

// brandString is the 48-byte, NUL-padded CPU name guests see.
var brandString = func() [48]byte {
	var b [48]byte
	copy(b[:], "Ksar Virtual CPU")
	return b
}()
