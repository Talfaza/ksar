package vmm

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"ksar/internal/kvm"
)

// ErrTimeout is returned when the guest did not finish before the deadline.
var ErrTimeout = errors.New("guest timed out")

// exitHandler handles one VM exit (run.ExitReason is set). It returns
// done=true to stop the VM.
type exitHandler func(vcpu *kvm.VCPU) (done bool, err error)

// runVCPU enters the guest repeatedly, passing each exit to handle, until
// the handler says done, an error occurs or the timeout expires. It owns
// the calling OS thread for its whole duration: KVM requires every KVM_RUN
// on a vCPU to come from the same thread, and the timeout signal targets it.
func runVCPU(vcpu *kvm.VCPU, timeout time.Duration, handle exitHandler) (exits int, err error) {
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
				return exits, ErrTimeout
			}
			continue // spurious interruption (e.g. Go runtime preemption)
		}
		if err != nil {
			return exits, fmt.Errorf("KVM_RUN: %w", err)
		}
		exits++
		done, err := handle(vcpu)
		if err != nil || done {
			return exits, err
		}
	}
}

// fatalExit describes exit reasons no handler can recover from.
func fatalExit(vcpu *kvm.VCPU) error {
	switch r := vcpu.RunState().ExitReason; r {
	case kvm.ExitFailEntry:
		return fmt.Errorf("vm entry failed: hardware reason %#x",
			vcpu.FailEntry().HardwareEntryFailureReason)
	case kvm.ExitInternalError:
		return fmt.Errorf("kvm internal error: suberror %d",
			vcpu.InternalError().Suberror)
	default:
		return fmt.Errorf("unhandled exit reason %d", r)
	}
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
