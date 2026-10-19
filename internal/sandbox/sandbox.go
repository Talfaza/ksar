// Package sandbox runs one command in a fresh, throwaway microVM: boot,
// copy files in, run, stream output, destroy.
package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"time"

	"ksar/internal/agent"
	"ksar/internal/agent/proto"
	"ksar/internal/initramfs"
	"ksar/internal/vmm"
)

// agentPath is where the agent lives in the guest. It sits outside the
// image's own directories so it never clobbers an image's /init.
const agentPath = ".ksar/agent"

// File is a file to place in the guest before the command runs.
type File struct {
	Path string // absolute path in the guest
	Mode uint32 // permission bits
	Data []byte
}

// Options configures a sandboxed run.
type Options struct {
	Kernel  []byte    // bzImage
	Agent   []byte    // static ksar-agent binary
	Image   io.Reader // optional root filesystem as a tar (e.g. docker export)
	MemSize int       // guest RAM in bytes (default 256 MiB)
	Timeout time.Duration
	Console io.Writer // if set, the guest kernel log is shown here (verbose)

	Files  []File
	Exec   proto.Exec
	Stdin  io.Reader // nil: empty stdin
	Stdout io.Writer
	Stderr io.Writer
}

// Result describes a finished run.
type Result struct {
	Exit        proto.Exit
	Boot        time.Duration // VM start until the agent was ready
	Total       time.Duration
	VM          vmm.LinuxResult
	ConsoleTail []byte // recent kernel/agent log, for diagnosing failures
}

// ErrTimeout is returned when the VM hit its timeout.
var ErrTimeout = vmm.ErrTimeout

// Run boots a VM, runs opts.Exec in it and destroys the VM. The returned
// error covers sandbox failures; the command's own failure is in
// Result.Exit.
func Run(opts Options) (Result, error) {
	var res Result
	start := time.Now()
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	rd, err := buildInitramfs(opts.Agent, opts.Image)
	if err != nil {
		return res, err
	}

	cmdline := vmm.DefaultCmdline + " rdinit=/" + agentPath
	if opts.Console == nil {
		cmdline += " quiet"
	}
	m, err := vmm.NewLinux(vmm.LinuxConfig{
		Kernel:       opts.Kernel,
		Initrd:       rd,
		Cmdline:      cmdline,
		MemSize:      opts.MemSize,
		Timeout:      opts.Timeout,
		Output:       opts.Console,
		AgentChannel: true,
	})
	if err != nil {
		return res, err
	}
	defer m.Close()

	// The session runs alongside the vCPU. Whichever ends first ends the
	// other: the session stops the VM when done, and a VM that stops
	// closes the channel, failing the session.
	type sessionResult struct {
		exit proto.Exit
		boot time.Duration
		err  error
	}
	done := make(chan sessionResult, 1)
	go func() {
		var s sessionResult
		s.exit, s.boot, s.err = session(m.Channel(), opts, start)
		m.Stop()
		done <- s
	}()

	vmRes, vmErr := m.Run()
	s := <-done
	res.VM, res.Exit, res.Boot = vmRes, s.exit, s.boot
	res.Total = time.Since(start)
	res.ConsoleTail = m.ConsoleTail()

	switch {
	case vmErr != nil:
		return res, vmErr
	case s.err != nil:
		return res, fmt.Errorf("%w (VM: %s)", s.err, vmRes.Reason)
	}
	return res, nil
}

func session(ch io.ReadWriter, opts Options, start time.Time) (proto.Exit, time.Duration, error) {
	c := agent.NewClient(ch)
	if _, err := c.WaitReady(); err != nil {
		return proto.Exit{}, 0, fmt.Errorf("waiting for agent: %w", err)
	}
	boot := time.Since(start)
	for _, f := range opts.Files {
		if err := c.PutFile(f.Path, f.Mode, bytes.NewReader(f.Data)); err != nil {
			return proto.Exit{}, boot, err
		}
	}
	exit, err := c.Run(opts.Exec, opts.Stdin, opts.Stdout, opts.Stderr)
	return exit, boot, err
}

func buildInitramfs(agentBin []byte, image io.Reader) ([]byte, error) {
	if len(agentBin) == 0 {
		return nil, errors.New("no agent binary")
	}
	var buf bytes.Buffer
	w := initramfs.NewWriter(&buf)
	if err := w.AddBase(); err != nil {
		return nil, err
	}
	if err := w.AddDir(".ksar", 0o755); err != nil {
		return nil, err
	}
	if err := w.AddFile(agentPath, 0o755, agentBin); err != nil {
		return nil, err
	}
	if image != nil {
		if err := w.AddTar(image); err != nil {
			return nil, fmt.Errorf("image: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
