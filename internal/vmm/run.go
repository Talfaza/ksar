package vmm

import (
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"ksar/internal/kvm"
)

// ErrTimeout is returned when the guest did not finish before the deadline.
var ErrTimeout = errors.New("guest timed out")

// errStopped is the stop reason when the host asked the VM to stop.
var errStopped = errors.New("stopped by host")

// exitHandler handles one VM exit (run.ExitReason is set). It returns
// done=true to stop the VM.
type exitHandler func(vcpu *kvm.VCPU) (done bool, err error)

// runner drives one vCPU and can be stopped from any goroutine.
type runner struct {
	vcpu *kvm.VCPU
	pid  int
	tid  atomic.Int32 // OS thread inside run(), 0 when not running

	mu     sync.Mutex
	reason error // first stop reason, nil until Stop is called
}

func newRunner(vcpu *kvm.VCPU) *runner {
	return &runner{vcpu: vcpu, pid: syscall.Getpid()}
}

// Stop makes run return as soon as possible with reason as its error
// (errStopped means a clean, host-requested stop). Safe from any goroutine,
// before or during run. Only the first reason is kept.
func (r *runner) Stop(reason error) {
	r.mu.Lock()
	if r.reason == nil {
		r.reason = reason
	}
	r.mu.Unlock()
	// ImmediateExit covers the race where the signal lands just before
	// KVM_RUN starts; the signal kicks a vCPU already inside the guest.
	// SIGURG is harmless to the Go runtime (it uses it for preemption).
	r.vcpu.RunState().ImmediateExit = 1
	if tid := r.tid.Load(); tid != 0 {
		syscall.Tgkill(r.pid, int(tid), syscall.SIGURG)
	}
}

func (r *runner) stopReason() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reason
}

// run enters the guest repeatedly, passing each exit to handle, until the
// handler says done, an error occurs, Stop is called or the timeout
// expires. It owns the calling OS thread for its whole duration: KVM
// requires every KVM_RUN on a vCPU to come from the same thread, and the
// stop signal targets it.
func (r *runner) run(timeout time.Duration, handle exitHandler) (exits int, err error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	r.tid.Store(int32(syscall.Gettid()))
	defer r.tid.Store(0)

	if timeout > 0 {
		timer := time.AfterFunc(timeout, func() { r.Stop(ErrTimeout) })
		defer timer.Stop()
	}

	for {
		if reason := r.stopReason(); reason != nil {
			return exits, reason
		}
		err := r.vcpu.Run()
		if errors.Is(err, syscall.EINTR) || errors.Is(err, syscall.EAGAIN) {
			continue // stopped, or a spurious signal (e.g. Go preemption)
		}
		if err != nil {
			return exits, fmt.Errorf("KVM_RUN: %w", err)
		}
		exits++
		done, err := handle(r.vcpu)
		if err != nil || done {
			return exits, err
		}
	}
}

// runVCPU runs a vCPU with only a timeout as the external stop.
func runVCPU(vcpu *kvm.VCPU, timeout time.Duration, handle exitHandler) (int, error) {
	return newRunner(vcpu).run(timeout, handle)
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

// tailBuffer keeps the last size bytes written to it.
type tailBuffer struct {
	mu   sync.Mutex
	size int
	buf  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - t.size; over > 0 {
		t.buf = append(t.buf[:0], t.buf[over:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) Bytes() []byte {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]byte(nil), t.buf...)
}
