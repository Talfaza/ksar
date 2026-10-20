// Package harden locks down the Ksar process before it runs untrusted
// guest code, so that a guest exploiting a bug in the VMM lands in a
// process that can barely do anything.
//
// Layers, applied by Lockdown:
//
//   - not dumpable and no core dumps: other processes of the same user
//     can't ptrace Ksar or read guest memory through /proc/PID/mem, and a
//     crash never writes guest memory to disk
//   - no_new_privs: nothing can regain privileges via setuid binaries
//   - seccomp: a syscall allowlist for the running VM (see seccomp.go):
//     no opening files, no new sockets, no exec, no new processes, no
//     executable memory
//
// Landlock is deliberately not used: it restricts only the calling
// thread, and by the time setup is done the Go runtime has many threads
// that would escape it. Seccomp's TSYNC flag covers every thread at once,
// and its allowlist already forbids opening files and sockets.
package harden

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

const (
	prSetDumpable   = 4
	prSetNoNewPrivs = 38
	prGetNoNewPrivs = 39
)

// CheckUser refuses to run as root. Ksar needs only access to /dev/kvm
// (the kvm group); as root, a VMM escape would own the whole machine.
func CheckUser(allowRoot bool) error {
	if os.Geteuid() == 0 && !allowRoot {
		return errors.New("refusing to run as root: a VM escape would get root on this machine. " +
			"Add your user to the kvm group instead (sudo usermod -aG kvm $USER), or pass -allow-root")
	}
	return nil
}

// Lockdown applies all process restrictions. It is irreversible and
// affects every thread. Call it after all files, devices and sockets the
// VM needs are open, and before guest code runs.
func Lockdown() error {
	if err := setrlimit(syscall.RLIMIT_CORE, 0); err != nil {
		return fmt.Errorf("disable core dumps: %w", err)
	}
	if err := prctl(prSetDumpable, 0); err != nil {
		return fmt.Errorf("PR_SET_DUMPABLE: %w", err)
	}
	if err := prctl(prSetNoNewPrivs, 1); err != nil {
		return fmt.Errorf("PR_SET_NO_NEW_PRIVS: %w", err)
	}
	return installSeccomp(vmmFilter())
}

func prctl(option, arg uintptr) error {
	_, _, e := syscall.RawSyscall6(syscall.SYS_PRCTL, option, arg, 0, 0, 0, 0)
	if e != 0 {
		return e
	}
	return nil
}

func setrlimit(resource int, v uint64) error {
	return syscall.Setrlimit(resource, &syscall.Rlimit{Cur: v, Max: v})
}
