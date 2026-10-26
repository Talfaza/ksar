package main

import (
	"os"
	"syscall"
	"unsafe"

	"ksar/internal/agent/proto"
)

const (
	rndAddEntropy  = 0x40085203 // RNDADDENTROPY
	rndReseedCRNG  = 0x5207     // RNDRESEEDCRNG
	maxHelloRandom = 512
)

// hello applies the host's first message: the run's id, and entropy that
// is mixed into the kernel RNG, which is then reseeded at once.
func hello(m proto.Hello) {
	if m.ID != "" && len(m.ID) <= 64 {
		os.MkdirAll("/run/ksar", 0o755)
		os.WriteFile("/run/ksar/id", []byte(m.ID+"\n"), 0o644)
	}
	if n := len(m.Entropy); n > 0 && n <= maxHelloRandom {
		f, err := os.OpenFile("/dev/urandom", os.O_WRONLY, 0)
		if err != nil {
			logf("reseed: %v", err)
			return
		}
		defer f.Close()
		// struct rand_pool_info { int entropy_count; int buf_size; __u32 buf[]; }
		buf := make([]byte, 8+n)
		*(*int32)(unsafe.Pointer(&buf[0])) = int32(n * 8)
		*(*int32)(unsafe.Pointer(&buf[4])) = int32(n)
		copy(buf[8:], m.Entropy)
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), rndAddEntropy, uintptr(unsafe.Pointer(&buf[0]))); e != 0 {
			logf("RNDADDENTROPY: %v", e)
		}
		if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), rndReseedCRNG, 0); e != 0 {
			logf("RNDRESEEDCRNG: %v", e)
		}
	}
}
