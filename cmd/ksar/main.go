// Command ksar runs untrusted code inside KVM microVMs.
package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"ksar/internal/guest"
	"ksar/internal/initramfs"
	"ksar/internal/vmm"
)

const usage = `ksar - run untrusted code in KVM microVMs

Usage:
  ksar boot [flags]                  boot a Linux kernel in a microVM (stage 2)
  ksar initramfs -init FILE -o OUT   build a minimal initramfs around an init binary
  ksar hello [-timeout 5s] [-spin]   run a tiny real-mode guest (stage 1)
  ksar help                          show this help

Run "ksar <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "boot":
		err = cmdBoot(os.Args[2:])
	case "initramfs":
		err = cmdInitramfs(os.Args[2:])
	case "hello":
		err = cmdHello(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "ksar: unknown command %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "ksar: %v\n", err)
		os.Exit(1)
	}
}

func cmdBoot(args []string) error {
	fs := flag.NewFlagSet("boot", flag.ExitOnError)
	kernel := fs.String("kernel", defaultKernel(), "bzImage to boot")
	initrd := fs.String("initrd", "", "initramfs to load (optional)")
	mem := fs.String("mem", "256M", "guest memory (e.g. 512M, 1G)")
	cmdline := fs.String("cmdline", vmm.DefaultCmdline, "kernel command line")
	appendArgs := fs.String("append", "", "extra kernel arguments added to -cmdline")
	timeout := fs.Duration("timeout", 30*time.Second, "kill the guest after this long (0 = never)")
	trace := fs.Bool("trace", false, "log guest accesses to devices that don't exist")
	fs.Parse(args)

	memSize, err := parseSize(*mem)
	if err != nil {
		return fmt.Errorf("-mem: %w", err)
	}
	kernelData, err := os.ReadFile(*kernel)
	if err != nil {
		return err
	}
	var initrdData []byte
	if *initrd != "" {
		if initrdData, err = os.ReadFile(*initrd); err != nil {
			return err
		}
	}
	cfg := vmm.LinuxConfig{
		Kernel:  kernelData,
		Initrd:  initrdData,
		Cmdline: strings.TrimSpace(*cmdline + " " + *appendArgs),
		MemSize: int(memSize),
		Timeout: *timeout,
		Output:  os.Stdout,
	}
	if *trace {
		cfg.Trace = os.Stderr
	}

	res, err := vmm.RunLinux(cfg)
	if errors.Is(err, vmm.ErrTimeout) {
		fmt.Fprintf(os.Stderr, "\n[ksar] guest killed after %v timeout\n", *timeout)
		os.Exit(124)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "\n[ksar] %s after %v (%d VM exits)\n",
		res.Reason, res.Duration.Round(time.Millisecond), res.Exits)
	if res.Truncated {
		fmt.Fprintln(os.Stderr, "[ksar] console output was truncated")
	}
	return nil
}

func cmdInitramfs(args []string) error {
	fs := flag.NewFlagSet("initramfs", flag.ExitOnError)
	initPath := fs.String("init", "", "static binary to run as /init (required)")
	outPath := fs.String("o", "", "output file (required)")
	fs.Parse(args)
	if *initPath == "" || *outPath == "" {
		fs.Usage()
		os.Exit(2)
	}
	init, err := os.ReadFile(*initPath)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := initramfs.Minimal(&buf, init); err != nil {
		return err
	}
	return os.WriteFile(*outPath, buf.Bytes(), 0o644)
}

func cmdHello(args []string) error {
	fs := flag.NewFlagSet("hello", flag.ExitOnError)
	timeout := fs.Duration("timeout", 5*time.Second, "kill the guest after this long (0 = never)")
	spin := fs.Bool("spin", false, "run a guest that loops forever, to see the timeout work")
	fs.Parse(args)

	code := guest.Hello
	if *spin {
		code = guest.Spin
	}
	res, err := vmm.RunRealMode(vmm.RealModeConfig{
		Code:    code,
		Timeout: *timeout,
		Output:  os.Stdout,
	})
	if errors.Is(err, vmm.ErrTimeout) {
		fmt.Fprintf(os.Stderr, "[ksar] guest killed after %v timeout\n", *timeout)
		os.Exit(124)
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "[ksar] guest exited with code %d after %d VM exits in %v\n",
		res.ExitCode, res.Exits, res.Duration.Round(time.Microsecond))
	if res.Truncated {
		fmt.Fprintln(os.Stderr, "[ksar] guest output was truncated")
	}
	return nil
}

// defaultKernel returns the running host kernel's bzImage path.
func defaultKernel() string {
	var u syscall.Utsname
	if err := syscall.Uname(&u); err != nil {
		return ""
	}
	var b strings.Builder
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		b.WriteByte(byte(c))
	}
	return "/boot/vmlinuz-" + b.String()
}

// parseSize parses sizes like "256M", "1G", "65536K" or plain bytes.
func parseSize(s string) (int64, error) {
	s = strings.ToUpper(strings.TrimSuffix(strings.TrimSpace(s), "B"))
	mult := int64(1)
	if n := len(s); n > 0 {
		switch s[n-1] {
		case 'K':
			mult, s = 1<<10, s[:n-1]
		case 'M':
			mult, s = 1<<20, s[:n-1]
		case 'G':
			mult, s = 1<<30, s[:n-1]
		}
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v <= 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return v * mult, nil
}
