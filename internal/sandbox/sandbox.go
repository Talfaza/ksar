// Package sandbox runs one command in a fresh, throwaway microVM: boot,
// copy files in, run, stream output, destroy.
package sandbox

import (
	"bytes"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"ksar/internal/agent"
	"ksar/internal/agent/proto"
	"ksar/internal/display"
	"ksar/internal/initramfs"
	"ksar/internal/netproxy"
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

// Dir is a directory to place in the guest: Tar is an archive of its
// contents (see PackDir), extracted into Path.
type Dir struct {
	Path string
	Tar  io.Reader
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
	Layers   []string  // Virtio: EROFS layers stacked on RootDisk, topmost first
	Image    io.Reader // legacy: root filesystem tar (optional)

	// Net, if set, gives the guest a network card (eth0, 10.0.2.15/24)
	// bridged to a network proxy. Virtio only. Nil means no network.
	Net *vmm.Net

	// Display, if set, gives the guest a screen, keyboard and tablet
	// (virtio mode only). Viewers connect over VNC to Listeners, which
	// must be open before the VM starts (lockdown forbids new sockets).
	// SaveChanges, if set, receives the guest's filesystem changes as a
	// tar (a layer for Layers) when the command exits 0. MaxSave bounds
	// its size (default 4 GiB). It must be open before the run, since
	// lockdown forbids opening files.
	SaveChanges io.Writer
	MaxSave     int64
	SaveResult  *SaveResult // filled in when SaveChanges is set

	Display   *vmm.Display
	Listeners []net.Listener
	VNCAuth   display.Auth

	// ID is the VM's id; the guest finds it in /run/ksar/id.
	ID string

	Timeout time.Duration
	Console io.Writer // if set, the guest kernel log is shown here (verbose)

	// Lockdown, if set, is called once the VM and its devices exist and
	// before any guest code runs. Use it to apply irreversible process
	// restrictions (see package harden).
	Lockdown func() error

	// OnReady, if set, is called once the guest agent is up, with the
	// boot time and the VM's vsock CID (0 in legacy mode).
	OnReady func(boot time.Duration, cid uint32)

	Files  []File
	Dirs   []Dir
	Exec   proto.Exec
	Stdin  io.Reader // nil: empty stdin
	Stdout io.Writer
	Stderr io.Writer
}

// SaveResult reports a SaveChanges export.
type SaveResult struct {
	Saved bool
	Files int
	Bytes int64
	Err   error
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
		if opts.Net != nil {
			cfg.Net = opts.Net
			// Static configuration by the kernel itself (CONFIG_IP_PNP):
			// address, gateway, netmask, hostname, device, no
			// autoconfiguration, DNS server. The agent turns the DNS entry
			// in /proc/net/pnp into /etc/resolv.conf.
			// The network is IPv4 only.
			cfg.Cmdline += fmt.Sprintf(" ipv6.disable=1 ip=%s::%s:%s:ksar:eth0:off:%s",
				netproxy.GuestAddr, netproxy.GatewayAddr, netproxy.Netmask, netproxy.DNSAddr)
		}
		cfg.Display = opts.Display
		if opts.RootDisk != "" {
			cfg.Disks = []vmm.Disk{{Path: opts.RootDisk, ReadOnly: true}}
			cfg.Cmdline += " ksar.root=/dev/vda"
			var devs []string
			for i, l := range opts.Layers {
				cfg.Disks = append(cfg.Disks, vmm.Disk{Path: l, ReadOnly: true})
				devs = append(devs, "/dev/vd"+string(rune('b'+i)))
			}
			if len(devs) > 0 {
				cfg.Cmdline += " ksar.layers=" + strings.Join(devs, ",")
			}
		} else if len(opts.Layers) > 0 || opts.SaveChanges != nil {
			return res, errors.New("layers and saving changes need an image")
		}
	} else {
		cfg.AgentChannel = true
		if opts.Net != nil {
			opts.Net.Backend.Close()
			return res, errors.New("networking needs the Ksar kernel (virtio mode)")
		}
		if opts.Display != nil {
			return res, errors.New("the display needs the Ksar kernel (virtio mode)")
		}
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
	if s := m.Screen(); s != nil {
		for _, l := range opts.Listeners {
			go serveViewers(l, s, opts.VNCAuth)
			defer l.Close()
		}
	}
	if opts.Lockdown != nil {
		if err := opts.Lockdown(); err != nil {
			return res, fmt.Errorf("lockdown: %w", err)
		}
	}

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
		s.exit, s.boot, s.err = session(ch, opts, start, m.GuestCID())
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

// maxViewers bounds concurrent VNC connections per VM.
const maxViewers = 4

func serveViewers(l net.Listener, s *display.Screen, auth display.Auth) {
	slots := make(chan struct{}, maxViewers)
	for {
		c, err := l.Accept()
		if err != nil {
			return // listener closed: the run is over
		}
		select {
		case slots <- struct{}{}:
			go func() {
				defer func() { <-slots }()
				s.Serve(c, auth)
			}()
		default:
			c.Close()
		}
	}
}

func session(ch io.ReadWriter, opts Options, start time.Time, cid uint32) (proto.Exit, time.Duration, error) {
	c := agent.NewClient(ch)
	ready, err := c.WaitReady()
	if err != nil {
		return proto.Exit{}, 0, fmt.Errorf("waiting for agent: %w", err)
	}
	entropy := make([]byte, 64)
	rand.Read(entropy)
	if err := c.Hello(opts.ID, entropy); err != nil {
		return proto.Exit{}, 0, err
	}
	for _, n := range ready.Notes {
		if len(n) <= 500 { // from the guest: printed as plain text, bounded
			fmt.Fprintf(opts.Stderr, "ksar: note: %s\n", n)
		}
	}
	boot := time.Since(start)
	if opts.OnReady != nil {
		opts.OnReady(boot, cid)
	}
	for _, f := range opts.Files {
		if err := c.PutFile(f.Path, f.Mode, bytes.NewReader(f.Data)); err != nil {
			return proto.Exit{}, boot, err
		}
	}
	for _, d := range opts.Dirs {
		if _, err := c.PutDir(d.Path, d.Tar); err != nil {
			return proto.Exit{}, boot, err
		}
	}
	exit, err := c.Run(opts.Exec, opts.Stdin, opts.Stdout, opts.Stderr)
	if err == nil && opts.SaveChanges != nil && exit.Code == 0 && exit.Signal == "" {
		max := opts.MaxSave
		if max <= 0 {
			max = 4 << 30
		}
		files, n, serr := c.Export(opts.SaveChanges, nil, max)
		if opts.SaveResult != nil {
			*opts.SaveResult = SaveResult{Saved: serr == nil, Files: files, Bytes: n, Err: serr}
		}
	}
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
