package main

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"ksar/internal/agent/proto"
	"ksar/internal/agentbin"
	imagecache "ksar/internal/image"
	"ksar/internal/sandbox"
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
	image := fs.String("image", "", "root filesystem tar, e.g. from `docker export` (default: agent only)")
	mem := fs.String("mem", "256M", "guest memory")
	timeout := fs.Duration("timeout", 60*time.Second, "kill the VM after this long (0 = never)")
	interactive := fs.Bool("i", false, "forward ksar's stdin to the command")
	workdir := fs.String("w", "", "working directory for the command")
	verbose := fs.Bool("v", false, "show the guest kernel log and timing")
	agentPath := fs.String("agent", "", "use this ksar-agent binary instead of the embedded one")
	var copies, envs multiFlag
	fs.Var(&copies, "copy", "copy a host file into the guest: HOST_PATH:GUEST_PATH (repeatable)")
	fs.Var(&envs, "e", "set an environment variable: KEY=VALUE (repeatable)")
	fs.Parse(args)

	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "ksar: %v\n", err)
		return exitKsarError
	}

	memSize, err := parseSize(*mem)
	if err != nil {
		return fail(fmt.Errorf("-mem: %w", err))
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
	files, err := parseCopies(copies)
	if err != nil {
		return fail(err)
	}

	virtioMode := isKsarKernel(kernelData) && !*legacy
	opts := sandbox.Options{
		Virtio:  virtioMode,
		Kernel:  kernelData,
		Agent:   agentData,
		MemSize: int(memSize),
		Timeout: *timeout,
		Files:   files,
		Exec:    proto.Exec{Args: fs.Args(), Env: envs, Dir: *workdir},
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
	if *verbose {
		opts.Console = os.Stderr
	}

	res, err := sandbox.Run(opts)
	if *verbose {
		mode := "legacy (serial)"
		if virtioMode {
			mode = "virtio (vsock)"
		}
		fmt.Fprintf(os.Stderr, "[ksar] %s: boot %v, total %v, VM: %s\n", mode,
			res.Boot.Round(time.Millisecond), res.Total.Round(time.Millisecond), res.VM.Reason)
	}
	if errors.Is(err, sandbox.ErrTimeout) {
		fmt.Fprintf(os.Stderr, "ksar: VM killed after %v timeout\n", *timeout)
		return exitTimeout
	}
	if err != nil {
		if !*verbose {
			showTail(res.ConsoleTail)
		}
		return fail(err)
	}
	if res.Exit.Error != "" {
		fmt.Fprintf(os.Stderr, "ksar: %s\n", res.Exit.Error)
	}
	return res.Exit.Code
}

// parseCopies reads each HOST:GUEST pair into memory.
func parseCopies(specs []string) ([]sandbox.File, error) {
	var files []sandbox.File
	for _, spec := range specs {
		host, guest, ok := strings.Cut(spec, ":")
		if !ok || host == "" || guest == "" {
			return nil, fmt.Errorf("-copy %q: want HOST_PATH:GUEST_PATH", spec)
		}
		if !path.IsAbs(guest) {
			return nil, fmt.Errorf("-copy %q: guest path must be absolute", spec)
		}
		st, err := os.Stat(host)
		if err != nil {
			return nil, err
		}
		if !st.Mode().IsRegular() {
			return nil, fmt.Errorf("-copy %q: only regular files can be copied", spec)
		}
		data, err := os.ReadFile(host)
		if err != nil {
			return nil, err
		}
		files = append(files, sandbox.File{Path: guest, Mode: uint32(st.Mode().Perm()), Data: data})
	}
	return files, nil
}

// showTail prints the last lines of the guest console after a failure.
func showTail(tail []byte) {
	tail = bytes.TrimSpace(tail)
	if len(tail) == 0 {
		return
	}
	lines := strings.Split(string(tail), "\n")
	if len(lines) > 25 {
		lines = lines[len(lines)-25:]
	}
	fmt.Fprintln(os.Stderr, "ksar: last guest console output:")
	for _, l := range lines {
		fmt.Fprintln(os.Stderr, "  | "+strings.TrimRight(l, "\r"))
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
