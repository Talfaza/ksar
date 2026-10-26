package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"ksar/internal/agent/proto"
	"ksar/internal/agentbin"
	"ksar/internal/desktop"
	"ksar/internal/harden"
	imagecache "ksar/internal/image"
	"ksar/internal/netproxy"
	"ksar/internal/registry"
	"ksar/internal/sandbox"
	"ksar/internal/termsafe"
)

// Exit codes for failures of ksar itself, following `docker run`.
const (
	exitKsarError = 125
	exitTimeout   = 124
)

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdRun(args []string) int {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: ksar run [flags] [--] COMMAND [ARGS...]

Runs COMMAND in a fresh microVM that is destroyed afterwards. ksar exits
with the command's exit code (124 on timeout, 125 if ksar itself failed).

Example:
  ksar run -image build/alpine.tar -- sh -c 'uname -a; id'

Flags:
`)
		fs.PrintDefaults()
	}
	kernel := fs.String("kernel", findKernel(), "bzImage to boot (default: the Ksar kernel if built, else the host's)")
	legacy := fs.Bool("legacy", false, "use the serial channel and an in-RAM image even with a Ksar kernel")
	image := fs.String("image", "", "root filesystem tar, e.g. from 'docker export' (default: agent only)")
	mem := fs.String("mem", "256M", "guest memory")
	timeout := fs.Duration("timeout", 60*time.Second, "kill the VM after this long (0 = never)")
	interactive := fs.Bool("i", false, "forward ksar's stdin to the command")
	workdir := fs.String("w", "", "working directory for the command")
	verbose := fs.Bool("v", false, "show the guest kernel log and timing")
	agentPath := fs.String("agent", "", "use this ksar-agent binary instead of the embedded one")
	lockdown := fs.Bool("lockdown", true, "restrict ksar with seccomp before the guest runs (disable only to debug)")
	allowRoot := fs.Bool("allow-root", false, "allow running as root (not recommended)")
	cgroup := fs.String("cgroup", "auto", "run in a systemd scope with memory/CPU/task limits: auto, on or off")
	raw := fs.Bool("raw", false, "pass guest output to the terminal unfiltered, including control sequences")
	eventsFD := fs.Int("events-fd", -1, "write machine-readable JSON events to this open file descriptor (for the web UI)")
	consoleFD := fs.Int("console-fd", -1, "write the guest kernel log to this open file descriptor (for the web UI)")
	displayFlag := fs.String("display", "", "give the guest a screen: on (1280x800) or WIDTHxHEIGHT; view it over VNC (see -vnc)")
	var vncAddrs multiFlag
	fs.Var(&vncAddrs, "vnc", "with -display: serve VNC on 127.0.0.1:PORT (password printed) or unix:PATH (repeatable; default 127.0.0.1:0, a free port)")
	idFlag := fs.String("id", "", "this VM's id, shown by ksar ps and used by ksar stop (default: a random 12-character id)")
	label := fs.String("label", "", "a short description shown by ksar ps instead of the command")
	desktopFlag := fs.Bool("desktop", false, "with -display: start X and a desktop, then run COMMAND on it (no COMMAND: just the desktop). "+
		"If the image has no desktop, a minimal XFCE is installed once, in a VM, and cached as a layer")
	saveChanges := fs.String("save-changes", "", "if COMMAND exits 0, save the guest's filesystem changes to this tar (usable with -layer)")
	var layerTars multiFlag
	fs.Var(&layerTars, "layer", "stack this tar (e.g. from -save-changes) on the image, read-only (repeatable, topmost first)")
	netMode := fs.String("net", "none", "network access: none (no network card), allow (only -allow hosts) or internet (any public address)")
	var copies, envs, allow multiFlag
	fs.Var(&allow, "allow", "with -net allow: a host the guest may reach, e.g. pypi.org, *.pythonhosted.org, github.com:443, 1.2.3.4, 10.0.0.0/8:8080 (repeatable, or comma-separated)")
	fs.Var(&copies, "copy", "copy a host file or directory into the guest: HOST_PATH:GUEST_PATH (repeatable)")
	fs.Var(&envs, "e", "set an environment variable: KEY=VALUE (repeatable)")
	fs.Parse(args)

	if fs.NArg() == 0 && !*desktopFlag {
		fs.Usage()
		return 2
	}
	ev := openEvents(*eventsFD)
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "ksar: %v\n", err)
		ev.emit("error", map[string]any{"message": err.Error(), "code": exitKsarError})
		return exitKsarError
	}
	if err := harden.CheckUser(*allowRoot); err != nil {
		return fail(err)
	}

	memSize, err := parseSize(*mem)
	if err != nil {
		return fail(fmt.Errorf("-mem: %w", err))
	}
	// Cgroup limits: guest RAM plus headroom for ksar itself, and in
	// legacy mode the image held in RAM while the initramfs is built.
	limits := cgroupLimits{MemoryMax: memSize + 256<<20, TasksMax: 256, CPUQuota: 150}
	if *image != "" && (*legacy || !isKsarKernelFile(*kernel)) {
		if st, err := os.Stat(*image); err == nil {
			limits.MemoryMax += st.Size() * 3
		}
	}
	// Every VM has an id. It is chosen once, here, and handed to the
	// re-executed ksar in its cgroup scope (see enterScope).
	vmID := *idFlag
	if vmID == "" {
		vmID = registry.NewID()
	}
	if !registry.ValidID(vmID) {
		return fail(fmt.Errorf("-id %q: use lower-case letters, digits and -, up to 32 characters", vmID))
	}
	entry := registry.Entry{ID: vmID, Status: "preparing", Image: *image, Command: fs.Args(), Label: *label,
		MemMB: int(memSize >> 20), Display: *displayFlag, Net: *netMode}
	if *desktopFlag && len(fs.Args()) == 0 && *label == "" {
		entry.Label = "desktop"
	}
	reg, err := registry.Register(entry)
	if err != nil {
		if strings.Contains(err.Error(), "already running") {
			return fail(err)
		}
		if *verbose {
			fmt.Fprintf(os.Stderr, "[ksar] not listed by ksar ps: %v\n", err)
		}
	}
	defer func() { // the exit status is set at the end of cmdRun
		if reg != nil && reg.Entry().Status != "exited" {
			reg.Update(func(e *registry.Entry) { e.Status = "exited" })
		}
	}()
	ev.emit("id", map[string]any{"id": vmID})
	// ksar stop sends SIGTERM; record how the VM ended before going.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		s := <-sigs
		code := 128 + int(s.(syscall.Signal))
		reg.Update(func(e *registry.Entry) { e.Status, e.ExitCode = "exited", &code })
		ev.emit("exit", map[string]any{"code": code})
		os.Exit(code)
	}()

	// The desktop install is its own ksar run (its own VM, scope and
	// lockdown), so it happens before this one sets up anything.
	execArgs := fs.Args()
	if *desktopFlag {
		if *displayFlag == "" {
			*displayFlag = "on"
		}
		if *image == "" {
			return fail(errors.New("-desktop needs -image"))
		}
		info, err := desktop.Inspect(*image)
		if err != nil {
			return fail(err)
		}
		if !info.Ready() {
			ev.emit("preparing", map[string]any{"what": "desktop"})
			layer, err := desktop.Ensure(*image, cliBuilder(*lockdown, *kernel), os.Stderr)
			if err != nil {
				return fail(err)
			}
			layerTars = append(layerTars, layer)
		}
		execArgs = desktop.Session(execArgs)
	}
	if err := enterScope(*cgroup, limits, *verbose, vmID); err != nil {
		return fail(err)
	}
	if *verbose {
		if s := currentScope(); s != "" {
			fmt.Fprintf(os.Stderr, "[ksar] cgroup scope %s (memory <= %d MiB, no swap, <= %d tasks, <= %d%% CPU)\n",
				s, limits.MemoryMax>>20, limits.TasksMax, limits.CPUQuota)
		}
	}
	kernelData, err := os.ReadFile(*kernel)
	if err != nil {
		return fail(err)
	}
	var agentData []byte
	if *agentPath != "" {
		agentData, err = os.ReadFile(*agentPath)
	} else {
		agentData, err = agentbin.Agent()
	}
	if err != nil {
		return fail(err)
	}
	for _, e := range envs {
		if !strings.Contains(e, "=") {
			return fail(fmt.Errorf("-e %q: want KEY=VALUE", e))
		}
	}
	files, dirs, err := parseCopies(copies, *verbose)
	if err != nil {
		return fail(err)
	}
	// The image's own environment and working directory (lost by docker
	// export, kept in NAME.tar.json) apply first; -e and -w override them.
	if *image != "" {
		cfg, err := imagecache.LoadConfig(*image)
		if err != nil {
			return fail(err)
		}
		envs = append(append(multiFlag{}, cfg.Env...), envs...)
		if *workdir == "" {
			*workdir = cfg.WorkingDir
		}
	}

	virtioMode := isKsarKernel(kernelData) && !*legacy
	policy, err := parseNet(*netMode, allow)
	if err != nil {
		return fail(err)
	}
	disp, err := parseDisplay(*displayFlag)
	if err != nil {
		return fail(err)
	}
	if disp == nil && len(vncAddrs) > 0 {
		return fail(errors.New("-vnc needs -display"))
	}
	if disp != nil && !virtioMode {
		return fail(errors.New("-display needs the Ksar kernel (build it with `make kernel`) and no -legacy"))
	}
	if policy != nil && !virtioMode {
		return fail(errors.New("-net needs the Ksar kernel (build it with `make kernel`) and no -legacy"))
	}
	netInfo := map[string]any{"mode": netproxy.ModeNone}
	if policy != nil {
		netInfo = map[string]any{"mode": policy.Mode, "allow": policy.Allow}
	}
	ev.emit("started", map[string]any{
		"pid": os.Getpid(), "scope": os.Getenv(scopeEnv), "virtio": virtioMode,
		"kernel": kernelVersion(kernelData), "mem": memSize, "mem_limit": limits.MemoryMax,
		"timeout_ms": timeout.Milliseconds(), "lockdown": *lockdown, "image": *image,
		"net": netInfo, "id": vmID,
	})
	reg.Update(func(e *registry.Entry) { e.Status, e.Scope = "booting", os.Getenv(scopeEnv) })
	if *verbose {
		fmt.Fprintf(os.Stderr, "[ksar] vm %s\n", vmID)
	}
	opts := sandbox.Options{
		Virtio:  virtioMode,
		Kernel:  kernelData,
		Agent:   agentData,
		MemSize: int(memSize),
		Timeout: *timeout,
		Files:   files,
		Dirs:    dirs,
		Exec:    proto.Exec{Args: execArgs, Env: envs, Dir: *workdir},
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
	}
	if *image != "" && virtioMode {
		start := time.Now()
		disk, err := imagecache.FromTar(*image)
		if err != nil {
			return fail(err)
		}
		opts.RootDisk = disk
		for _, l := range layerTars {
			d, err := imagecache.FromTar(l)
			if err != nil {
				return fail(fmt.Errorf("-layer %s: %w", l, err))
			}
			opts.Layers = append(opts.Layers, d)
		}
		if *verbose {
			fmt.Fprintf(os.Stderr, "[ksar] image %s ready in %v\n", disk, time.Since(start).Round(time.Millisecond))
		}
	} else if *image != "" {
		f, err := os.Open(*image)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		opts.Image = bufio.NewReaderSize(f, 1<<20)
		// Legacy mode holds the archive and its unpacked copy in RAM at once.
		if st, err := f.Stat(); err == nil && st.Size()*5/2+128<<20 > memSize {
			fmt.Fprintf(os.Stderr, "ksar: warning: legacy mode unpacks the %d MiB image into RAM; "+
				"-mem %dM or more is recommended\n", st.Size()>>20, (st.Size()*5/2+128<<20)>>20)
		}
	}
	if *interactive {
		opts.Stdin = os.Stdin
	}
	if disp != nil {
		opts.Display = disp
		if len(vncAddrs) == 0 {
			vncAddrs = multiFlag{"127.0.0.1:0"}
		}
		ls, auth, urls, err := listenVNC(vncAddrs)
		if err != nil {
			return fail(err)
		}
		opts.Listeners, opts.VNCAuth = ls, auth
		ev.emit("display", map[string]any{"width": disp.Width, "height": disp.Height, "vnc": urls})
		reg.Update(func(e *registry.Entry) { e.VNC = urls })
		if *eventsFD < 0 {
			for _, u := range urls {
				if auth.Password != "" {
					fmt.Fprintf(os.Stderr, "[ksar] display %dx%d: %s (password %s)\n", disp.Width, disp.Height, u, auth.Password)
				} else {
					fmt.Fprintf(os.Stderr, "[ksar] display %dx%d: %s\n", disp.Width, disp.Height, u)
				}
			}
		}
	}
	if policy != nil {
		// Blocked attempts are worth seeing on the terminal; everything is
		// in the event stream. Under the UI (events fd) stderr belongs to
		// the guest, so nothing is printed there.
		onEvent := func(e netproxy.Event) {
			ev.emit("net", map[string]any{"net": e})
			if *eventsFD < 0 && (*verbose || !e.Allowed) {
				fmt.Fprintf(os.Stderr, "[ksar] net: %s\n", netproxy.FormatEvent(e))
			}
		}
		if opts.Net, err = startNetProxy(*policy, *lockdown && !builtWithCgo, onEvent); err != nil {
			return fail(err)
		}
		if *verbose {
			fmt.Fprintf(os.Stderr, "[ksar] network: %s %v (guest %s, DNS %s)\n",
				policy.Mode, policy.Allow, netproxy.GuestAddr, netproxy.DNSAddr)
		}
	}
	if *lockdown && builtWithCgo {
		return fail(errors.New("this ksar was built with cgo, which the seccomp lockdown does not support; " +
			"rebuild with `make ksar` (CGO_ENABLED=0)"))
	}
	if *lockdown {
		opts.Lockdown = harden.Lockdown
		if os.Getenv("KSAR_SECCOMP_DEBUG") != "" {
			fmt.Fprintln(os.Stderr, "ksar: warning: KSAR_SECCOMP_DEBUG weakens the lockdown (blocked calls fail instead of killing ksar); debug only")
		}
	} else {
		fmt.Fprintln(os.Stderr, "ksar: warning: running without lockdown; a VMM bug would expose this machine")
	}
	// Untrusted output headed for a terminal is filtered (pipes and files
	// get exact bytes). The check must happen before lockdown, which
	// forbids the ioctl it uses.
	stdout, stderr := guestOutput(os.Stdout, *raw), guestOutput(os.Stderr, *raw)
	opts.Stdout, opts.Stderr = stdout, stderr
	if *verbose {
		opts.Console = guestOutput(os.Stderr, *raw)
	}
	if *consoleFD >= 0 {
		f := os.NewFile(uintptr(*consoleFD), "console")
		if f == nil {
			return fail(fmt.Errorf("-console-fd %d is not open", *consoleFD))
		}
		if opts.Console != nil {
			opts.Console = io.MultiWriter(opts.Console, f)
		} else {
			opts.Console = f
		}
	}
	opts.OnReady = func(boot time.Duration, cid uint32) {
		ev.emit("ready", map[string]any{"boot_ms": boot.Milliseconds(), "cid": cid})
		reg.Update(func(e *registry.Entry) { e.Status, e.CID = "running", cid })
	}
	opts.ID = vmID

	defer func() {
		removed := 0
		for _, w := range []io.Writer{stdout, stderr, opts.Console} {
			if t, ok := w.(*termsafe.Writer); ok {
				t.Flush()
				removed += t.Removed()
			}
		}
		if removed > 0 {
			fmt.Fprintf(os.Stderr, "ksar: removed %d terminal control sequence(s) from the guest's output (-raw keeps them)\n", removed)
		}
	}()
	var saved sandbox.SaveResult
	if *saveChanges != "" {
		if !virtioMode || *image == "" {
			return fail(errors.New("-save-changes needs -image and the Ksar kernel"))
		}
		// Opened now: lockdown forbids opening (or removing) files later.
		f, err := os.Create(*saveChanges)
		if err != nil {
			return fail(err)
		}
		defer f.Close()
		opts.SaveChanges, opts.SaveResult = f, &saved
	}
	if len(layerTars) > 0 && opts.RootDisk == "" {
		return fail(errors.New("-layer needs -image and the Ksar kernel"))
	}
	code := runSandbox(opts, *verbose, *raw, *timeout, virtioMode, fail)
	if *saveChanges != "" {
		switch {
		case saved.Saved:
			if *verbose {
				fmt.Fprintf(os.Stderr, "[ksar] saved %d changed files (%s) to %s\n", saved.Files, humanBytes(saved.Bytes), *saveChanges)
			}
		case saved.Err != nil:
			fmt.Fprintf(os.Stderr, "ksar: saving changes failed: %v (%s is incomplete)\n", saved.Err, *saveChanges)
			if code == 0 {
				code = exitKsarError
			}
		default:
			fmt.Fprintf(os.Stderr, "ksar: changes not saved: the command failed (%s is empty)\n", *saveChanges)
		}
	}
	ev.emit("exit", map[string]any{"code": code})
	reg.Update(func(e *registry.Entry) { e.Status, e.ExitCode = "exited", &code })
	return code
}

// runSandbox runs the VM and maps the outcome to an exit code.
func runSandbox(opts sandbox.Options, verbose, raw bool, timeout time.Duration, virtioMode bool,
	fail func(error) int) int {

	res, err := sandbox.Run(opts)
	if verbose {
		mode := "legacy (serial)"
		if virtioMode {
			mode = "virtio (vsock)"
		}
		fmt.Fprintf(os.Stderr, "[ksar] %s: boot %v, total %v, VM: %s\n", mode,
			res.Boot.Round(time.Millisecond), res.Total.Round(time.Millisecond), res.VM.Reason)
	}
	if errors.Is(err, sandbox.ErrTimeout) {
		fmt.Fprintf(os.Stderr, "ksar: VM killed after %v timeout\n", timeout)
		return exitTimeout
	}
	if err != nil {
		if !verbose {
			showTail(res.ConsoleTail, raw)
		}
		return fail(err)
	}
	if res.Exit.Error != "" {
		fmt.Fprintf(os.Stderr, "ksar: %s\n", res.Exit.Error)
	}
	return res.Exit.Code
}

// parseCopies prepares each HOST:GUEST pair. Files are read into memory;
// directories are packed into an unlinked temporary file. Both happen
// before lockdown, which forbids opening files; the open temp file is
// then streamed into the guest.
func parseCopies(specs []string, verbose bool) ([]sandbox.File, []sandbox.Dir, error) {
	var files []sandbox.File
	var dirs []sandbox.Dir
	for _, spec := range specs {
		host, guest, ok := strings.Cut(spec, ":")
		if !ok || host == "" || guest == "" {
			return nil, nil, fmt.Errorf("-copy %q: want HOST_PATH:GUEST_PATH", spec)
		}
		if !path.IsAbs(guest) {
			return nil, nil, fmt.Errorf("-copy %q: guest path must be absolute", spec)
		}
		st, err := os.Stat(host)
		if err != nil {
			return nil, nil, err
		}
		switch {
		case st.IsDir():
			tmp, err := os.CreateTemp("", "ksar-copy-*.tar")
			if err != nil {
				return nil, nil, err
			}
			os.Remove(tmp.Name()) // gone from the filesystem; the fd keeps it alive
			bw := bufio.NewWriterSize(tmp, 1<<20)
			ps, err := sandbox.PackDir(bw, host)
			if err == nil {
				err = bw.Flush()
			}
			if err == nil {
				_, err = tmp.Seek(0, io.SeekStart)
			}
			if err != nil {
				tmp.Close()
				return nil, nil, fmt.Errorf("-copy %q: %w", spec, err)
			}
			dirs = append(dirs, sandbox.Dir{Path: guest, Tar: bufio.NewReaderSize(tmp, 1<<20)})
			if verbose {
				fmt.Fprintf(os.Stderr, "[ksar] %s -> %s: %d files (%s), %d dirs, %d symlinks (not followed), %d special files skipped\n",
					host, guest, ps.Files, humanBytes(ps.Bytes), ps.Dirs, ps.Symlinks, ps.Skipped)
			}
		case st.Mode().IsRegular():
			data, err := os.ReadFile(host)
			if err != nil {
				return nil, nil, err
			}
			files = append(files, sandbox.File{Path: guest, Mode: uint32(st.Mode().Perm()), Data: data})
		default:
			return nil, nil, fmt.Errorf("-copy %q: only regular files and directories can be copied", spec)
		}
	}
	return files, dirs, nil
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// showTail prints the last lines of the guest console after a failure.
func showTail(tail []byte, raw bool) {
	tail = bytes.TrimSpace(tail)
	if len(tail) == 0 {
		return
	}
	lines := strings.Split(string(tail), "\n")
	if len(lines) > 25 {
		lines = lines[len(lines)-25:]
	}
	fmt.Fprintln(os.Stderr, "ksar: last guest console output:")
	out := guestOutput(os.Stderr, raw)
	for _, l := range lines {
		fmt.Fprintln(out, "  | "+strings.TrimRight(l, "\r"))
	}
	if t, ok := out.(*termsafe.Writer); ok {
		t.Flush()
	}
}

// findKernel picks the default kernel: the Ksar guest kernel if one is
// built or installed, otherwise the running host kernel.
func findKernel() string {
	candidates := []string{os.Getenv("KSAR_KERNEL")}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), "build", "bzImage"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		candidates = append(candidates, filepath.Join(home, ".local", "share", "ksar", "bzImage"))
	}
	for _, c := range candidates {
		if c == "" {
			continue
		}
		if st, err := os.Stat(c); err == nil && st.Mode().IsRegular() {
			return c
		}
	}
	return defaultKernel()
}

// isKsarKernel reports whether a bzImage is the Ksar guest kernel, by its
// version string (CONFIG_LOCALVERSION="-ksar"). The setup header stores a
// pointer to that string at 0x20e, relative to 0x200.
func isKsarKernel(k []byte) bool {
	if len(k) < 0x210 || string(k[0x202:0x206]) != "HdrS" {
		return false
	}
	off := 0x200 + int(binary.LittleEndian.Uint16(k[0x20e:]))
	if off >= len(k) {
		return false
	}
	end := bytes.IndexByte(k[off:min(len(k), off+256)], 0)
	if end < 0 {
		return false
	}
	version, _, _ := strings.Cut(string(k[off:off+end]), " ")
	return strings.HasSuffix(version, "-ksar")
}

// isKsarKernelFile is isKsarKernel for a path, reading only the header.
func isKsarKernelFile(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	head := make([]byte, 64<<10)
	n, _ := io.ReadFull(f, head)
	return isKsarKernel(head[:n])
}

// guestOutput wraps f in a control-sequence filter when it is a terminal
// and raw is not set.
func guestOutput(f *os.File, raw bool) io.Writer {
	if raw || !isTerminal(f) {
		return f
	}
	return termsafe.NewWriter(f)
}

// isTerminal reports whether f is a tty (TCGETS succeeds).
func isTerminal(f *os.File) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}
