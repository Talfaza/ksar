// Package vmm drives KVM virtual machines: memory layout, vCPU setup and
// the exit-handling loop.
package vmm

import (
	"errors"
	"fmt"
	"io"
	"time"

	"ksar/internal/guest"
	"ksar/internal/kvm"
)

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
	res.Exits, err = runVCPU(vcpu, cfg.Timeout, func(vcpu *kvm.VCPU) (bool, error) {
		switch vcpu.RunState().ExitReason {
		case kvm.ExitIO:
			return handleRealModeIO(vcpu, out, &res)
		case kvm.ExitHLT:
			res.Halted = true
			return true, nil
		case kvm.ExitShutdown:
			return true, errors.New("guest shut down (triple fault)")
		}
		return true, fatalExit(vcpu)
	})
	res.Duration = time.Since(start)
	res.OutputBytes, res.Truncated = out.written, out.truncated
	return res, err
}

// handleRealModeIO emulates the two stage-1 ports. It reports done=true
// when the guest asked to exit. Unknown ports behave like empty hardware:
// writes are dropped and reads return 0xff.
func handleRealModeIO(vcpu *kvm.VCPU, out *limitedWriter, res *Result) (done bool, err error) {
	d := vcpu.IO()
	data, err := vcpu.IOData()
	if err != nil {
		return true, err
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
			return true, fmt.Errorf("write serial output: %w", err)
		}
	case guest.ExitPort:
		if len(data) > 0 {
			res.ExitCode = int(data[0])
		}
		return true, nil
	}
	return false, nil
}
