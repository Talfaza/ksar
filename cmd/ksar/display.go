package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"

	"ksar/internal/desktop"
	"ksar/internal/display"
	"ksar/internal/vmm"
)

// parseDisplay parses -display: "" (none), "on", or WIDTHxHEIGHT.
func parseDisplay(s string) (*vmm.Display, error) {
	switch s {
	case "", "off", "none":
		return nil, nil
	case "on":
		return &vmm.Display{Width: 1280, Height: 800}, nil
	}
	ws, hs, ok := strings.Cut(strings.ToLower(s), "x")
	w, err1 := strconv.Atoi(ws)
	h, err2 := strconv.Atoi(hs)
	if !ok || err1 != nil || err2 != nil {
		return nil, fmt.Errorf("-display %q: want on or WIDTHxHEIGHT, e.g. 1280x800", s)
	}
	if w < 320 || h < 200 || w > vmm.MaxDisplayWidth || h > vmm.MaxDisplayHeight || w%8 != 0 {
		return nil, fmt.Errorf("-display %q: width and height must be between 320x200 and %dx%d, width a multiple of 8",
			s, vmm.MaxDisplayWidth, vmm.MaxDisplayHeight)
	}
	return &vmm.Display{Width: w, Height: h}, nil
}

// listenVNC opens the VNC listeners before lockdown. TCP is loopback
// only and gets a random password (any local user could connect to it);
// a Unix socket is protected by its file permissions instead.
func listenVNC(addrs []string) ([]net.Listener, display.Auth, []string, error) {
	var ls []net.Listener
	var urls []string
	var auth display.Auth
	fail := func(err error) ([]net.Listener, display.Auth, []string, error) {
		for _, l := range ls {
			l.Close()
		}
		return nil, auth, nil, err
	}
	for _, a := range addrs {
		if path, ok := strings.CutPrefix(a, "unix:"); ok {
			if len(path) > 107 {
				return fail(fmt.Errorf("-vnc %s: Unix socket paths are limited to 107 bytes; use a shorter one", a))
			}
			old := syscall.Umask(0o077) // the socket is created mode 0600
			l, err := net.Listen("unix", path)
			syscall.Umask(old)
			if err != nil {
				return fail(fmt.Errorf("-vnc %s: %w", a, err))
			}
			// Not unlinked on close: by then ksar is locked down and may not
			// delete files. The socket's owner (e.g. ksar ui) removes it.
			l.(*net.UnixListener).SetUnlinkOnClose(false)
			ls = append(ls, l)
			urls = append(urls, "unix:"+path)
			continue
		}
		host, _, err := net.SplitHostPort(a)
		if err != nil {
			return fail(fmt.Errorf("-vnc %s: want 127.0.0.1:PORT or unix:PATH", a))
		}
		if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
			return fail(fmt.Errorf("-vnc %s: VNC is only served on loopback (127.0.0.1 or ::1)", a))
		}
		// No keep-alive: setting it needs setsockopt calls the lockdown
		// doesn't allow on accepted connections.
		lc := net.ListenConfig{KeepAlive: -1}
		l, err := lc.Listen(context.Background(), "tcp", a)
		if err != nil {
			return fail(fmt.Errorf("-vnc %s: %w", a, err))
		}
		ls = append(ls, l)
		urls = append(urls, "vnc://"+l.Addr().String())
		if auth.Password == "" {
			auth.Password = display.NewPassword()
		}
	}
	if len(ls) == 0 {
		return fail(errors.New("no VNC address"))
	}
	return ls, auth, urls, nil
}

// cliBuilder runs the desktop install as a child ksar run, its output on
// our stderr (filtered by the child when that is a terminal).
func cliBuilder(lockdown bool, kernel string) desktop.Builder {
	return func(args []string, log io.Writer) error {
		self, err := os.Executable()
		if err != nil {
			return err
		}
		argv := append([]string{"run", fmt.Sprintf("-lockdown=%v", lockdown)}, args...)
		if kernel != "" {
			argv = append([]string{"run", "-kernel", kernel}, argv[1:]...)
		}
		cmd := exec.Command(self, argv...)
		cmd.Stdout, cmd.Stderr = log, log
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("install VM: %w", err)
		}
		return nil
	}
}
