// Package vmm drives KVM virtual machines: memory layout, vCPU setup and
// the exit-handling loop.
package vmm

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"ksar/internal/guest"
	"ksar/internal/kvm"
)

// ErrTimeout is returned when the guest did not finish before the deadline.
var ErrTimeout = errors.New("guest timed out")

// RealModeConfig describes a stage-1 run: a flat binary executed in 16-bit
// real mode with no devices except a serial port and an exit port.
type RealModeConfig struct {
	Code      []byte        // program bytes, loaded at guest.LoadAddr
	MemSize   int           // guest RAM in bytes (default 2 MiB)
	Timeout   time.Duration // 0 means no timeout
	Output    io.Writer     // receives serial port output (may be nil)
	MaxOutput int64         // cap on serial output bytes (default 1 MiB)
}

// Result reports how a guest run ended.
type Result struct {
	ExitCode    int  // value written to the exit port, or -1
	Halted      bool // guest executed HLT
	OutputBytes int64
	Truncated   bool // guest produced more than MaxOutput
	Exits       int  // number of VM exits handled
	Duration    time.Duration
}

// RunRealMode creates a VM, runs cfg.Code on one vCPU and tears it all down.
func RunRealMode(cfg RealModeConfig) (Result, error) {
	res := Result{ExitCode: -1}
	if cfg.MemSize == 0 {
		cfg.MemSize = 2 << 20
	}
	if cfg.MaxOutput == 0 {
		cfg.MaxOutput = 1 << 20
	}
	if cfg.Output == nil {
		cfg.Output = io.Discard
	}
	if len(cfg.Code) == 0 || guest.LoadAddr+len(cfg.Code) > cfg.MemSize {
		return res, fmt.Errorf("program size %d does not fit in guest memory", len(cfg.Code))
	}

	sys, err := kvm.Open()
	if err != nil {
		return res, err
	}
	defer sys.Close()

	vm, err := sys.CreateVM()
	if err != nil {
		return res, err
	}
	defer vm.Close()

	mem, err := vm.AddMemory(0, 0, cfg.MemSize)
	if err != nil {
		return res, err
	}
	copy(mem[guest.LoadAddr:], cfg.Code)

	vcpu, err := vm.CreateVCPU(0)
	if err != nil {
		return res, err
	}

	// A fresh vCPU starts at the x86 reset vector (CS base 0xffff0000).
	// Point CS at 0 so execution starts at our flat program.
	sregs, err := vcpu.GetSregs()
	if err != nil {
		return res, err
	}
	sregs.CS.Base, sregs.CS.Selector = 0, 0
	if err := vcpu.SetSregs(sregs); err != nil {
		return res, err
	}
	if err := vcpu.SetRegs(kvm.Regs{
		RIP:    guest.LoadAddr,
		RFLAGS: 0x2, // bit 1 is reserved and must be set
	}); err != nil {
		return res, err
	}

	out := &limitedWriter{w: cfg.Output, remaining: cfg.MaxOutput}
	start := time.Now()
	err = runLoop(vcpu, cfg.Timeout, out, &res)
	res.Duration = time.Since(start)
	res.OutputBytes, res.Truncated = out.written, out.truncated
	return res, err
}

// runLoop enters the guest repeatedly and handles each exit. It owns the
// calling OS thread for its whole duration: KVM requires every KVM_RUN on a
// vCPU to come from the same thread, and the timeout signal targets it.
func runLoop(vcpu *kvm.VCPU, timeout time.Duration, out *limitedWriter, res *Result) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	run := vcpu.RunState()
	var expired atomic.Bool
	if timeout > 0 {
		pid, tid := syscall.Getpid(), syscall.Gettid()
		timer := time.AfterFunc(timeout, func() {
			expired.Store(true)
			// ImmediateExit covers the race where the signal lands just
			// before KVM_RUN starts; the signal kicks a vCPU already inside
			// the guest. SIGURG is harmless to the Go runtime (it uses it
			// for preemption itself).
			run.ImmediateExit = 1
			syscall.Tgkill(pid, tid, syscall.SIGURG)
		})
		defer timer.Stop()
	}

	for {
		err := vcpu.Run()
		if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) {
			if expired.Load() {
				return ErrTimeout
			}
			continue // spurious interruption (e.g. Go runtime preemption)
		}
		if err != nil {
			return fmt.Errorf("KVM_RUN: %w", err)
		}
		res.Exits++

		switch run.ExitReason {
		case kvm.ExitIO:
			done, err := handleIO(vcpu, out, res)
			if err != nil || done {
				return err
			}
		case kvm.ExitHLT:
			res.Halted = true
			return nil
		case kvm.ExitShutdown:
			return errors.New("guest shut down (triple fault)")
		case kvm.ExitFailEntry:
			return fmt.Errorf("vm entry failed: hardware reason %#x",
				vcpu.FailEntry().HardwareEntryFailureReason)
		case kvm.ExitInternalError:
			return fmt.Errorf("kvm internal error: suberror %d",
				vcpu.InternalError().Suberror)
		default:
			return fmt.Errorf("unhandled exit reason %d", run.ExitReason)
		}
	}
}

// handleIO emulates port I/O. It reports done=true when the guest asked to
// exit. Unknown ports behave like empty hardware: writes are dropped and
// reads return 0xff.
func handleIO(vcpu *kvm.VCPU, out *limitedWriter, res *Result) (done bool, err error) {
	d := vcpu.IO()
	data, err := vcpu.IOData()
	if err != nil {
		return false, err
	}
	if d.Direction == kvm.IODirIn {
		for i := range data {
			data[i] = 0xff
		}
		return false, nil
	}
	switch d.Port {
	case guest.SerialPort:
		// Size is 1 for byte OUTs; with Count > 1 (rep outsb) data holds
		// every byte in order.
		if _, err := out.Write(data); err != nil {
			return false, fmt.Errorf("write serial output: %w", err)
		}
	case guest.ExitPort:
		if len(data) > 0 {
			res.ExitCode = int(data[0])
		}
		return true, nil
	}
	return false, nil
}

// limitedWriter forwards at most remaining bytes, then silently drops the
// rest, so a guest cannot flood the host with output.
type limitedWriter struct {
	w         io.Writer
	remaining int64
	written   int64
	truncated bool
}

func (l *limitedWriter) Write(p []byte) (int, error) {
	n := len(p)
	if int64(len(p)) > l.remaining {
		p = p[:l.remaining]
		l.truncated = true
	}
	if len(p) > 0 {
		m, err := l.w.Write(p)
		l.remaining -= int64(m)
		l.written += int64(m)
		if err != nil {
			return m, err
		}
	}
	return n, nil
}
