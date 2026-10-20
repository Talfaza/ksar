package harden

import (
	"fmt"
	"os"
	"syscall"
	"unsafe"
)

// Seccomp filter for the running VMM (x86_64 only).
//
// Everything a running VM needs: the vCPU loop (KVM_RUN), interrupt
// injection, device I/O on already-open fds, vhost setup (the guest driver
// activates vsock after boot), the agent connection (accept on an
// already-listening socket), and what the Go runtime itself uses.
// Anything else is a violation and kills the process with SIGSYS, whose
// Go traceback shows which call was blocked.

// BPF opcodes.
const (
	bpfLdWAbs = 0x20 // A = *(u32 *)(data + k)
	bpfJeqK   = 0x15
	bpfJsetK  = 0x45
	bpfJgeK   = 0x35
	bpfAndK   = 0x54
	bpfRetK   = 0x06

	retAllow = 0x7fff0000
	retTrap  = 0x00030000 // SIGSYS
	retKill  = 0x80000000 // SECCOMP_RET_KILL_PROCESS

	auditArchX86_64 = 0xc000003e
	x32SyscallBit   = 0x40000000

	// struct seccomp_data offsets.
	offNr   = 0
	offArch = 4
	offArgs = 16
)

func argLow(i int) uint32  { return uint32(offArgs + 8*i) }
func argHigh(i int) uint32 { return uint32(offArgs + 8*i + 4) }

type sockFilter struct {
	Code uint16
	Jt   uint8
	Jf   uint8
	K    uint32
}

type program []sockFilter

func (p *program) ld(off uint32) { *p = append(*p, sockFilter{Code: bpfLdWAbs, K: off}) }
func (p *program) ret(v uint32)  { *p = append(*p, sockFilter{Code: bpfRetK, K: v}) }
func (p *program) and(v uint32)  { *p = append(*p, sockFilter{Code: bpfAndK, K: v}) }
func (p *program) jeq(v uint32, t, f int) {
	*p = append(*p, sockFilter{Code: bpfJeqK, K: v, Jt: uint8(t), Jf: uint8(f)})
}
func (p *program) jset(v uint32, t, f int) {
	*p = append(*p, sockFilter{Code: bpfJsetK, K: v, Jt: uint8(t), Jf: uint8(f)})
}
func (p *program) jge(v uint32, t, f int) {
	*p = append(*p, sockFilter{Code: bpfJgeK, K: v, Jt: uint8(t), Jf: uint8(f)})
}

// cond emits "if nr == syscall { block }". The block must end every path
// with a return, since it overwrites the accumulator; the skip distance
// is computed from the block, never written by hand.
func (p *program) cond(nr uint32, build func(b *program)) {
	var b program
	build(&b)
	if len(b) > 255 || len(b) == 0 || b[len(b)-1].Code != bpfRetK {
		panic("seccomp: bad conditional block")
	}
	p.jeq(nr, 0, len(b))
	*p = append(*p, b...)
}

// Allowed syscalls with no argument checks (x86_64 numbers).
var allowed = map[string]uint32{
	// I/O on already-open fds.
	"read": 0, "write": 1, "close": 3, "fstat": 5, "lseek": 8,
	"pread64": 17, "pwrite64": 18, "fcntl": 72, "fsync": 74, "fdatasync": 75,
	// Memory (mmap and mprotect are checked separately).
	"munmap": 11, "brk": 12, "mremap": 25, "madvise": 28,
	// Signals: the Go runtime's preemption and the vCPU stop kick.
	"rt_sigaction": 13, "rt_sigprocmask": 14, "rt_sigreturn": 15,
	"sigaltstack": 131, "getpid": 39, "gettid": 186, "tgkill": 234,
	// Scheduling, time, threads.
	"sched_yield": 24, "nanosleep": 35, "futex": 202, "sched_getaffinity": 204,
	"clock_gettime": 228, "clock_nanosleep": 230, "restart_syscall": 219,
	"exit": 60, "exit_group": 231, "getrandom": 318,
	// Go's network poller (os.File on non-blocking fds, eventfd relays).
	"epoll_wait": 232, "epoll_ctl": 233, "epoll_pwait": 281,
	"epoll_create1": 291, "eventfd2": 290,
	// The agent connection: accept on the listening vsock socket.
	"accept4": 288, "select": 23, "pselect6": 270,
}

const (
	nrMmap     = 9
	nrMprotect = 10
	nrIoctl    = 16
	nrClone    = 56
	nrClone3   = 435

	protExec    = 0x4
	cloneThread = 0x10000
	errnoENOSYS = 38
)

// Allowed ioctl requests: KVM_RUN and KVM_IRQ_LINE on the VM, and any
// vhost request (type 0xAF) since the guest driver configures vsock
// while the VM runs.
var allowedIoctls = []uint32{
	0xAE80,     // KVM_RUN
	0x4008AE61, // KVM_IRQ_LINE
}

const vhostIoctlType = 0xAF00

func vmmFilter() program {
	var p program
	// Only native x86_64 syscalls; the x32 ABI is another way in.
	p.ld(offArch)
	p.jeq(auditArchX86_64, 1, 0)
	p.ret(retKill)
	p.ld(offNr)
	p.jge(x32SyscallBit, 0, 1)
	p.ret(retKill)

	for _, nr := range sortedAllowed() {
		p.jeq(nr, 0, 1)
		p.ret(retAllow)
	}

	// clone3: report "not implemented" so nothing can use it to bypass the
	// clone flag check below (its flags live in memory, out of BPF's reach).
	p.jeq(nrClone3, 0, 1)
	p.ret(syscallErrno(errnoENOSYS))

	// clone: threads only (CLONE_THREAD), never new processes.
	p.cond(nrClone, func(b *program) {
		b.ld(argLow(0))
		b.jset(cloneThread, 0, 1)
		b.ret(retAllow)
		b.ret(retTrap)
	})

	// mmap and mprotect: never executable memory.
	for _, nr := range []uint32{nrMmap, nrMprotect} {
		p.cond(nr, func(b *program) {
			b.ld(argLow(2))
			b.jset(protExec, 0, 1)
			b.ret(retTrap)
			b.ret(retAllow)
		})
	}

	// ioctl: listed KVM requests and vhost.
	p.cond(nrIoctl, func(b *program) {
		b.ld(argHigh(1))
		b.jeq(0, 1, 0)
		b.ret(retTrap)
		b.ld(argLow(1))
		for _, req := range allowedIoctls {
			b.jeq(req, 0, 1)
			b.ret(retAllow)
		}
		b.and(0xff00)
		b.jeq(vhostIoctlType, 0, 1)
		b.ret(retAllow)
		b.ret(retTrap)
	})

	if os.Getenv("KSAR_SECCOMP_DEBUG") != "" {
		// Diagnostic mode: instead of killing, make a blocked syscall
		// fail with errno 1000+nr, so whatever reports the error names
		// the call. Never use this with untrusted guests.
		for nr := uint32(0); nr < 460; nr++ {
			p.jeq(nr, 0, 1)
			p.ret(syscallErrno(1000 + nr))
		}
	}
	p.ret(retTrap)
	return p
}

func syscallErrno(e uint32) uint32 { return 0x00050000 | e } // SECCOMP_RET_ERRNO

// sortedAllowed returns allowlist numbers in a fixed order, so the
// generated program is deterministic.
func sortedAllowed() []uint32 {
	nrs := make([]uint32, 0, len(allowed))
	for _, nr := range allowed {
		nrs = append(nrs, nr)
	}
	for i := 1; i < len(nrs); i++ {
		for j := i; j > 0 && nrs[j] < nrs[j-1]; j-- {
			nrs[j], nrs[j-1] = nrs[j-1], nrs[j]
		}
	}
	return nrs
}

const (
	seccompSetModeFilter   = 1
	seccompFilterFlagTSync = 1
	sysSeccomp             = 317
)

// installSeccomp loads the filter into every thread of the process.
func installSeccomp(p program) error {
	if len(p) > 4096 {
		return fmt.Errorf("seccomp program too long (%d)", len(p))
	}
	prog := struct {
		Len    uint16
		_      [6]byte
		Filter *sockFilter
	}{Len: uint16(len(p)), Filter: &p[0]}
	r, _, e := syscall.RawSyscall(sysSeccomp, seccompSetModeFilter, seccompFilterFlagTSync,
		uintptr(unsafe.Pointer(&prog)))
	if e != 0 {
		return fmt.Errorf("seccomp: %w", e)
	}
	if r != 0 {
		return fmt.Errorf("seccomp: thread %d could not be synchronized", r)
	}
	return nil
}
