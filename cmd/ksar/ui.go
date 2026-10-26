package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"ksar/internal/harden"
	"ksar/internal/ui"
)

func cmdUI(args []string) int {
	fs := flag.NewFlagSet("ui", flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: ksar ui [flags]

Serves the Ksar web interface on this machine only, and prints a one-time
sign-in link. Every run started from the page is a separate, locked-down
'ksar run' process.

Flags:
`)
		fs.PrintDefaults()
	}
	addr := fs.String("addr", "127.0.0.1:8765", "listen address (loopback only; port 0 picks a free one)")
	images := fs.String("images", defaultDataDir("images"), "directory of root filesystem tars shown as images")
	kernel := fs.String("kernel", "", "kernel for runs (default: what ksar run picks)")
	open := fs.Bool("open", false, "open the sign-in link in the default browser")
	history := fs.String("history", defaultDataDir("runs"), "directory where finished runs are kept across restarts (\"\" = don't keep them)")
	allowRoot := fs.Bool("allow-root", false, "allow running as root (not recommended)")
	fs.Parse(args)

	fail := func(err error) int {
		fmt.Fprintf(os.Stderr, "ksar: %v\n", err)
		return exitKsarError
	}
	if err := harden.CheckUser(*allowRoot); err != nil {
		return fail(err)
	}
	if builtWithCgo {
		return fail(errors.New("this ksar was built with cgo; runs need the seccomp lockdown, so rebuild with `make ksar`"))
	}
	self, err := os.Executable()
	if err != nil {
		return fail(err)
	}
	if err := os.MkdirAll(*images, 0o755); err != nil {
		return fail(err)
	}
	if abs, err := filepath.Abs(*images); err == nil {
		*images = abs
	}
	kernelPath := *kernel
	if kernelPath == "" {
		kernelPath = findKernel()
	}

	cfg := ui.Config{Addr: *addr, KsarPath: self, Kernel: *kernel, ImagesDir: *images, HistoryDir: *history,
		Status: func() ui.Status { return machineStatus(kernelPath) }}
	srv, err := ui.New(cfg)
	if err != nil && strings.HasSuffix(*addr, ":8765") && errors.Is(err, syscall.EADDRINUSE) {
		cfg.Addr = strings.TrimSuffix(*addr, "8765") + "0" // default port busy: pick a free one
		srv, err = ui.New(cfg)
	}
	if err != nil {
		return fail(err)
	}

	st := machineStatus(kernelPath)
	fmt.Fprintf(os.Stderr, "Ksar UI on %s\n\n  Open: %s\n\n", srv.Origin(), srv.URL())
	fmt.Fprintln(os.Stderr, "  This link signs you in. Keep it private; restart ksar ui to get a new one.")
	if !st.KVM || !st.VhostVsock {
		fmt.Fprintln(os.Stderr, "  warning: /dev/kvm or /dev/vhost-vsock is not accessible; runs will fail.")
	}
	if !st.KsarKernel {
		fmt.Fprintln(os.Stderr, "  note: the Ksar kernel is not built (make kernel); runs use legacy mode.")
	}
	fmt.Fprintln(os.Stderr, "  Press Ctrl-C to stop (running VMs are stopped too).")
	if *open && (os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != "") {
		if xdg, err := exec.LookPath("xdg-open"); err == nil {
			exec.Command(xdg, srv.URL()).Start()
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Serve(ctx); err != nil {
		return fail(err)
	}
	fmt.Fprintln(os.Stderr, "\nKsar UI stopped.")
	return 0
}

// defaultDataDir is ksar's data directory for name ($XDG_DATA_HOME/ksar/NAME).
func defaultDataDir(name string) string {
	if dir := os.Getenv("XDG_DATA_HOME"); dir != "" {
		return filepath.Join(dir, "ksar", name)
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "share", "ksar", name)
	}
	return name
}

func machineStatus(kernelPath string) ui.Status {
	canOpen := func(p string) bool {
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return false
		}
		f.Close()
		return true
	}
	have := func(bin string) bool { _, err := exec.LookPath(bin); return err == nil }
	st := ui.Status{
		KVM: canOpen("/dev/kvm"), VhostVsock: canOpen("/dev/vhost-vsock"),
		Kernel: kernelPath, Cgroups: userManagerReachable(),
		MkfsErofs: have("mkfs.erofs"), Docker: have("docker"), Lockdown: true, PID: os.Getpid(),
	}
	if f, err := os.Open(kernelPath); err == nil {
		head := make([]byte, 64<<10)
		n, _ := f.Read(head)
		f.Close()
		st.KernelVersion = kernelVersion(head[:n])
		st.KsarKernel = isKsarKernel(head[:n])
	}
	return st
}
