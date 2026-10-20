package harden

import (
	"encoding/binary"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

// run interprets a classic BPF seccomp program against one syscall, so
// the filter's logic is tested without installing it.
func (p program) run(t *testing.T, arch, nr uint32, args ...uint64) uint32 {
	t.Helper()
	data := make([]byte, 64)
	binary.LittleEndian.PutUint32(data[offNr:], nr)
	binary.LittleEndian.PutUint32(data[offArch:], arch)
	for i, a := range args {
		binary.LittleEndian.PutUint64(data[offArgs+8*i:], a)
	}
	var acc uint32
	for pc := 0; pc < len(p); pc++ {
		ins := p[pc]
		switch ins.Code {
		case bpfLdWAbs:
			acc = binary.LittleEndian.Uint32(data[ins.K:])
		case bpfAndK:
			acc &= ins.K
		case bpfRetK:
			return ins.K
		case bpfJeqK, bpfJsetK, bpfJgeK:
			var cond bool
			switch ins.Code {
			case bpfJeqK:
				cond = acc == ins.K
			case bpfJsetK:
				cond = acc&ins.K != 0
			default:
				cond = acc >= ins.K
			}
			if cond {
				pc += int(ins.Jt)
			} else {
				pc += int(ins.Jf)
			}
			if pc+1 >= len(p) {
				t.Fatalf("jump past end of program at %d", pc)
			}
		default:
			t.Fatalf("unknown opcode %#x", ins.Code)
		}
	}
	t.Fatal("program fell off the end")
	return 0
}

func TestFilterLogic(t *testing.T) {
	p := vmmFilter()
	const x86 = auditArchX86_64
	cases := []struct {
		name string
		arch uint32
		nr   uint32
		args []uint64
		want uint32
	}{
		{"read", x86, 0, nil, retAllow},
		{"futex", x86, 202, nil, retAllow},
		{"openat", x86, 257, nil, retTrap},
		{"open", x86, 2, nil, retTrap},
		{"execve", x86, 59, nil, retTrap},
		{"socket", x86, 41, nil, retTrap},
		{"connect", x86, 42, nil, retTrap},
		{"ptrace", x86, 101, nil, retTrap},
		{"fork", x86, 57, nil, retTrap},
		{"clone thread", x86, nrClone, []uint64{0x50f00}, retAllow},
		{"clone process", x86, nrClone, []uint64{0x11}, retTrap},
		{"clone3", x86, nrClone3, nil, syscallErrno(errnoENOSYS)},
		{"mmap rw", x86, nrMmap, []uint64{0, 4096, 3}, retAllow},
		{"mmap exec", x86, nrMmap, []uint64{0, 4096, 7}, retTrap},
		{"mprotect exec", x86, nrMprotect, []uint64{0, 4096, 5}, retTrap},
		{"KVM_RUN", x86, nrIoctl, []uint64{5, 0xAE80}, retAllow},
		{"KVM_IRQ_LINE", x86, nrIoctl, []uint64{5, 0x4008AE61}, retAllow},
		{"vhost", x86, nrIoctl, []uint64{5, 0x4008AF61}, retAllow},
		{"KVM_SET_USER_MEMORY_REGION", x86, nrIoctl, []uint64{5, 0x4020AE46}, retTrap},
		{"KVM_CREATE_VM", x86, nrIoctl, []uint64{5, 0xAE01}, retTrap},
		{"TIOCSTI (type into tty)", x86, nrIoctl, []uint64{0, 0x5412}, retTrap},
		{"ioctl high bits", x86, nrIoctl, []uint64{5, 1<<32 | 0xAE80}, retTrap},
		{"x32 read", x86, x32SyscallBit | 0, nil, retKill},
		{"i386 arch", 0x40000003, 0, nil, retKill},
	}
	for _, c := range cases {
		if got := p.run(t, c.arch, c.nr, c.args...); got != c.want {
			t.Errorf("%s: got %#x, want %#x", c.name, got, c.want)
		}
	}
}

// The real filter is installed in a child process (it is irreversible).
// The child reports what happened; blocked calls kill it with SIGSYS.
func TestMain(m *testing.M) {
	if probe := os.Getenv("KSAR_HARDEN_PROBE"); probe != "" {
		runProbe(probe)
		return
	}
	os.Exit(m.Run())
}

func runProbe(probe string) {
	// The accept probe needs a listening socket created before lockdown,
	// like the VMM's agent listener.
	var lfd int
	if probe == "accept" {
		var err error
		lfd, err = syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM|syscall.SOCK_NONBLOCK, 0)
		if err != nil {
			os.Stdout.WriteString(err.Error())
			return
		}
		syscall.Bind(lfd, &syscall.SockaddrInet4{Addr: [4]byte{127, 0, 0, 1}})
		syscall.Listen(lfd, 1)
	}
	if err := Lockdown(); err != nil {
		os.Stdout.WriteString("lockdown failed: " + err.Error())
		os.Exit(3)
	}
	switch probe {
	case "runtime":
		// The Go runtime must keep working: goroutines, timers, GC, threads.
		done := make(chan int)
		for i := 0; i < 8; i++ {
			go func() {
				runtime.LockOSThread() // forces new OS threads (clone)
				x := make([]byte, 1<<20)
				time.Sleep(10 * time.Millisecond)
				done <- len(x)
			}()
		}
		for i := 0; i < 8; i++ {
			<-done
		}
		runtime.GC()
		// no_new_privs needs no separate check: the kernel refuses an
		// unprivileged seccomp filter unless it is set, so Lockdown
		// succeeding proves it.
		os.Stdout.WriteString("ok")
		return
	case "poller":
		// First use of Go's network poller after lockdown, as vsock does.
		fd, _, e := syscall.RawSyscall(syscall.SYS_EVENTFD2, 0, syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
		if e != 0 {
			os.Stdout.WriteString("eventfd: " + e.Error())
			return
		}
		f := os.NewFile(fd, "ev")
		go func() { time.Sleep(20 * time.Millisecond); syscall.Write(int(fd), []byte{1, 0, 0, 0, 0, 0, 0, 0}) }()
		var b [8]byte
		if _, err := f.Read(b[:]); err != nil {
			os.Stdout.WriteString("read: " + err.Error())
			return
		}
		f.Close()
		os.Stdout.WriteString("ok")
		return
	case "accept":
		for i := 0; i < 20; i++ {
			_, _, e := syscall.Syscall6(syscall.SYS_ACCEPT4, uintptr(lfd), 0, 0, syscall.SOCK_NONBLOCK, 0, 0)
			if e != syscall.EAGAIN {
				os.Stdout.WriteString("accept: " + e.Error())
				return
			}
			var set syscall.FdSet
			set.Bits[lfd/64] |= 1 << (uint(lfd) % 64)
			tv := syscall.NsecToTimeval(int64(5 * time.Millisecond))
			if _, err := syscall.Select(lfd+1, &set, nil, nil, &tv); err != nil {
				os.Stdout.WriteString("select: " + err.Error())
				return
			}
		}
		os.Stdout.WriteString("ok")
		return
	case "open":
		os.Open("/etc/passwd")
	case "prctl":
		// Even reading process flags is off the list after lockdown.
		syscall.RawSyscall(syscall.SYS_PRCTL, prGetNoNewPrivs, 0, 0)
	case "socket":
		syscall.Socket(syscall.AF_INET, syscall.SOCK_STREAM, 0)
	case "exec":
		syscall.Exec("/bin/true", []string{"true"}, nil)
	case "fork":
		syscall.RawSyscall(syscall.SYS_FORK, 0, 0, 0)
	case "execmem":
		syscall.Mmap(-1, 0, 4096, syscall.PROT_READ|syscall.PROT_WRITE|syscall.PROT_EXEC,
			syscall.MAP_PRIVATE|syscall.MAP_ANONYMOUS)
	}
	os.Stdout.WriteString("not blocked")
}

// linksLibc reports whether this test binary loaded glibc (cgo, which the
// race detector forces). Lockdown is unsupported there for the same reason
// ksar must be built with CGO_ENABLED=0: glibc creates threads using
// syscalls the filter blocks, with signals masked.
func linksLibc() bool {
	maps, err := os.ReadFile("/proc/self/maps")
	return err == nil && strings.Contains(string(maps), "libc.so")
}

func probe(t *testing.T, name string) (string, error) {
	t.Helper()
	if linksLibc() {
		t.Skip("test binary uses cgo/glibc (e.g. -race); lockdown needs a pure Go binary")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "KSAR_HARDEN_PROBE="+name)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestLockdownKeepsRuntimeWorking(t *testing.T) {
	out, err := probe(t, "runtime")
	if err != nil || out != "ok" {
		t.Fatalf("probe: %v\n%s", err, out)
	}
}

func TestLockdownBlocks(t *testing.T) {
	for _, name := range []string{"open", "socket", "exec", "fork", "execmem", "prctl"} {
		out, err := probe(t, name)
		if err == nil || strings.Contains(out, "not blocked") {
			t.Errorf("%s was not blocked: %v\n%s", name, err, out)
			continue
		}
		if !strings.Contains(out, "SIGSYS") {
			t.Errorf("%s: expected death by SIGSYS, got: %v\n%s", name, err, out)
		}
	}
}
