package boot

import "testing"

// FuzzLoad loads arbitrary "kernel images". The header fields steer
// where data is copied in guest memory, so this checks that no header
// value can make Load write outside it or panic.
func FuzzLoad(f *testing.F) {
	f.Add(fakeKernel()[:0x1000], "console=ttyS0", []byte("initrd"))
	f.Fuzz(func(t *testing.T, kernel []byte, cmdline string, initrd []byte) {
		mem := make([]byte, 48<<20)
		Load(mem, Linux{Kernel: kernel, Initrd: initrd, Cmdline: cmdline})
	})
}
