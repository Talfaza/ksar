// Package sandbox runs one command in a fresh, throwaway microVM: boot,
// copy files in, run, stream output, destroy.
package sandbox

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sync"
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
	Kernel  []byte // bzImage
	Agent   []byte // static ksar-agent binary
	MemSize int    // guest RAM in bytes (default 256 MiB)

	// Virtio selects the fast path: the agent talks over vsock and the
	// root filesystem is RootDisk (a read-only EROFS image, see package
	// image) under a RAM overlay. It needs the Ksar guest kernel.
	// Without Virtio, the agent uses a serial port and Image (a tar) is
	// unpacked into the initramfs, which works with any kernel.
	Virtio   bool
	RootDisk string    // Virtio: EROFS image path (optional: agent only)
	Image    io.Reader // legacy: root filesystem tar (optional)

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

	cfg := vmm.LinuxConfig{
		Kernel:  opts.Kernel,
		Cmdline: vmm.DefaultCmdline + " rdinit=/" + agentPath,
		MemSize: opts.MemSize,
		Timeout: opts.Timeout,
		Output:  opts.Console,
	}
	if opts.Console == nil {
		cfg.Cmdline += " quiet"
	}
	tarball := opts.Image
	if opts.Virtio {
		tarball = nil
		cfg.Vsock = true
		if opts.RootDisk != "" {
			cfg.Disks = []vmm.Disk{{Path: opts.RootDisk, ReadOnly: true}}
			cfg.Cmdline += " ksar.root=/dev/vda"
		}
	} else {
		cfg.AgentChannel = true
	}
	var err error
	if cfg.Initrd, err = buildInitramfs(opts.Agent, tarball); err != nil {
		return res, err
	}
	m, err := vmm.NewLinux(cfg)
	if err != nil {
		return res, err
	}
	defer m.Close()

	// The vsock connection must be closed once the VM stops, or a session
	// blocked reading from a dead guest would never return.
	var connMu sync.Mutex
	var conn io.Closer
	vmStopped := false

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
		defer func() { m.Stop(); done <- s }()
		var ch io.ReadWriter = m.Channel()
		if opts.Virtio {
			c, err := m.AgentConn()
			if err != nil {
				s.err = fmt.Errorf("waiting for agent: %w", err)
				return
			}
			connMu.Lock()
			conn = c
			stopped := vmStopped
			connMu.Unlock()
			if stopped {
				c.Close()
			}
			ch = c
		}
		s.exit, s.boot, s.err = session(ch, opts, start)
	}()

	vmRes, vmErr := m.Run()
	connMu.Lock()
	vmStopped = true
	if conn != nil {
		conn.Close()
	}
	connMu.Unlock()
	s := <-done
	res.VM, res.Exit, res.Boot = vmRes, s.exit, s.boot
	res.Total = time.Since(start)
	res.ConsoleTail = m.ConsoleTail()

	// The kernel keeps booting after a failed initramfs unpack (usually
	// out of RAM), leaving a partial filesystem. Never report results from
	// such a guest as if it were healthy.
	if bytes.Contains(res.ConsoleTail, []byte("Initramfs unpacking failed")) {
		return res, errors.New("the guest ran out of memory unpacking the image into RAM; " +
			"increase -mem (legacy mode needs about 2.5x the image size)")
	}

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
