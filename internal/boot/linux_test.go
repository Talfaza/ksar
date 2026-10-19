package boot

import (
	"encoding/binary"
	"strings"
	"testing"

	"ksar/internal/kvm"
)

// fakeKernel builds a minimal image with a valid setup header.
func fakeKernel() []byte {
	k := make([]byte, 64<<10)
	k[hdrSetupSects] = 4
	k[hdrJumpLen] = 0x66 // header ends at 0x268
	copy(k[hdrMagic:], "HdrS")
	binary.LittleEndian.PutUint16(k[hdrVersion:], 0x020f)
	k[hdrLoadFlags] = loadedHigh
	binary.LittleEndian.PutUint16(k[hdrXLoadFlags:], xlfKernel64)
	binary.LittleEndian.PutUint32(k[hdrCmdlineSize:], 2047)
	binary.LittleEndian.PutUint32(k[hdrInitSize:], 8<<20)
	binary.LittleEndian.PutUint32(k[hdrInitrdAddrMax:], 0x7fffffff)
	copy(k[5*512:], "PMKERNEL")
	return k
}

func TestLoad(t *testing.T) {
	mem := make([]byte, 64<<20)
	initrd := []byte("INITRD-CONTENTS")
	entry, err := Load(mem, Linux{Kernel: fakeKernel(), Initrd: initrd, Cmdline: "console=ttyS0"})
	if err != nil {
		t.Fatal(err)
	}
	if entry != KernelAddr+0x200 {
		t.Errorf("entry = %#x", entry)
	}
	if got := string(mem[KernelAddr : KernelAddr+8]); got != "PMKERNEL" {
		t.Errorf("protected-mode kernel not at 1 MiB: %q", got)
	}
	zp := mem[zeroPageAddr:]
	if string(zp[hdrMagic:hdrMagic+4]) != "HdrS" {
		t.Error("setup header not copied into boot_params")
	}
	if zp[hdrTypeOfLoader] != 0xff {
		t.Error("type_of_loader not set")
	}
	cmdPtr := binary.LittleEndian.Uint32(zp[hdrCmdLinePtr:])
	if got := string(mem[cmdPtr : cmdPtr+14]); got != "console=ttyS0\x00" {
		t.Errorf("cmdline = %q", got)
	}
	rdAddr := binary.LittleEndian.Uint32(zp[hdrRamdiskImage:])
	rdSize := binary.LittleEndian.Uint32(zp[hdrRamdiskSize:])
	if rdAddr%4096 != 0 || int(rdSize) != len(initrd) || int(rdAddr)+len(initrd) > len(mem) {
		t.Errorf("initrd at %#x size %d", rdAddr, rdSize)
	}
	if string(mem[rdAddr:rdAddr+rdSize]) != string(initrd) {
		t.Error("initrd contents not copied")
	}
	if n := zp[bpE820Entries]; n != 2 {
		t.Fatalf("e820 entries = %d", n)
	}
	end := binary.LittleEndian.Uint64(zp[bpE820Table+20:]) + binary.LittleEndian.Uint64(zp[bpE820Table+28:])
	if end != uint64(len(mem)) {
		t.Errorf("e820 RAM ends at %#x, want %#x", end, len(mem))
	}
	// Page tables identity-map 4 GiB: check the entry for 3 GiB + 2 MiB.
	pde := binary.LittleEndian.Uint64(mem[pdAddr+3*0x1000+8:])
	if pde&^0xfff != 3<<30|2<<20 || pde&0x83 != 0x83 {
		t.Errorf("PD entry = %#x", pde)
	}
}

func TestLoadRejectsBadInput(t *testing.T) {
	mem := make([]byte, 64<<20)
	cases := map[string]func(k []byte) ([]byte, Linux){
		"not a bzImage": func(k []byte) ([]byte, Linux) {
			copy(k[hdrMagic:], "XXXX")
			return mem, Linux{Kernel: k}
		},
		"old protocol": func(k []byte) ([]byte, Linux) {
			binary.LittleEndian.PutUint16(k[hdrVersion:], 0x0200)
			return mem, Linux{Kernel: k}
		},
		"no 64-bit entry": func(k []byte) ([]byte, Linux) {
			binary.LittleEndian.PutUint16(k[hdrXLoadFlags:], 0)
			return mem, Linux{Kernel: k}
		},
		"cmdline too long": func(k []byte) ([]byte, Linux) {
			return mem, Linux{Kernel: k, Cmdline: strings.Repeat("x", 3000)}
		},
		"too little memory": func(k []byte) ([]byte, Linux) {
			return make([]byte, 20<<20), Linux{Kernel: k}
		},
		"truncated": func(k []byte) ([]byte, Linux) {
			return mem, Linux{Kernel: k[:100]}
		},
	}
	for name, mk := range cases {
		m, l := mk(fakeKernel())
		if _, err := Load(m, l); err == nil {
			t.Errorf("%s: Load succeeded", name)
		}
	}
}

func TestGuestCPUID(t *testing.T) {
	in := []kvm.CPUIDEntry{
		{Function: 1, EBX: 0xff123456},
		{Function: 0x80000002}, {Function: 0x80000003}, {Function: 0x80000004},
	}
	out := guestCPUID(in, 3)
	if in[0].EBX != 0xff123456 {
		t.Error("input table was modified")
	}
	if out[0].EBX != 0x03123456 || out[0].ECX&(1<<31) == 0 {
		t.Errorf("leaf 1: ebx=%#x ecx=%#x", out[0].EBX, out[0].ECX)
	}
	var brand []byte
	for _, e := range out[1:] {
		for _, r := range []uint32{e.EAX, e.EBX, e.ECX, e.EDX} {
			brand = binary.LittleEndian.AppendUint32(brand, r)
		}
	}
	if got := strings.TrimRight(string(brand), "\x00"); got != "Ksar Virtual CPU" {
		t.Errorf("brand = %q", got)
	}
}
