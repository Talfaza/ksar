package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"ksar/internal/harden"
	"ksar/internal/netproxy"
	"ksar/internal/vmm"
)

// proxyFD is where the proxy finds its end of the frame socket.
const proxyFD = 3

// cmdNetProxy is the network proxy process. ksar run starts it; it is
// not meant to be run by hand. Frames arrive on fd 3; events go to stdout
// as JSON lines, the first being {"kind":"ready"}.
func cmdNetProxy(args []string) int {
	fs := flag.NewFlagSet("net-proxy", flag.ExitOnError)
	policy := fs.String("policy", "", "network policy (JSON)")
	lockdown := fs.Bool("lockdown", true, "restrict the proxy with seccomp")
	fs.Parse(args)

	p, err := netproxy.DecodePolicy(*policy)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ksar net-proxy: bad policy: %v\n", err)
		return 2
	}
	c, err := netproxy.Compile(p)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ksar net-proxy: %v\n", err)
		return 2
	}
	if *lockdown {
		if err := harden.LockdownProxy(); err != nil {
			fmt.Fprintf(os.Stderr, "ksar net-proxy: lockdown: %v\n", err)
			return 1
		}
	}
	var mu sync.Mutex
	enc := json.NewEncoder(os.Stdout)
	emit := func(e netproxy.Event) {
		mu.Lock()
		enc.Encode(e)
		mu.Unlock()
	}
	emit(netproxy.Event{Kind: "ready", Allowed: true})
	if err := netproxy.New(c, emit).Serve(proxyFD); err != nil {
		fmt.Fprintf(os.Stderr, "ksar net-proxy: %v\n", err)
		return 1
	}
	return 0
}

// parseNet turns -net and -allow into a policy. It returns nil for
// "none" (no network card at all).
func parseNet(mode string, allow multiFlag) (*netproxy.Policy, error) {
	var rules []string
	for _, a := range allow {
		for _, r := range strings.Split(a, ",") {
			if r = strings.TrimSpace(r); r != "" {
				rules = append(rules, r)
			}
		}
	}
	switch mode {
	case netproxy.ModeNone, "":
		if len(rules) > 0 {
			return nil, errors.New("-allow needs -net allow (or -net internet)")
		}
		return nil, nil
	case netproxy.ModeAllow, netproxy.ModeInternet:
	default:
		return nil, fmt.Errorf("-net %q: want none, allow or internet", mode)
	}
	p := &netproxy.Policy{Mode: mode, Allow: rules}
	if _, err := netproxy.Compile(*p); err != nil { // fail here, not in the child
		return nil, fmt.Errorf("-net: %w", err)
	}
	return p, nil
}

// startNetProxy launches the proxy process for one VM and returns the
// VMM's end of the frame socket. Every event the proxy reports is passed
// to onEvent. The proxy is killed if ksar dies, and exits by itself when
// the VM's end of the socket closes.
func startNetProxy(p netproxy.Policy, lockdown bool, onEvent func(netproxy.Event)) (*vmm.Net, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("network socket: %w", err)
	}
	// Room for a burst of full-size frames in each direction.
	for _, fd := range fds {
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_SNDBUF, 1<<20)
		syscall.SetsockoptInt(fd, syscall.SOL_SOCKET, syscall.SO_RCVBUF, 1<<20)
	}
	if err := syscall.SetNonblock(fds[0], true); err != nil {
		syscall.Close(fds[0])
		syscall.Close(fds[1])
		return nil, err
	}
	vmEnd := os.NewFile(uintptr(fds[0]), "net-vm")
	proxyEnd := os.NewFile(uintptr(fds[1]), "net-proxy")
	defer proxyEnd.Close()

	self, err := os.Executable()
	if err != nil {
		vmEnd.Close()
		return nil, err
	}
	cmd := exec.Command(self, "net-proxy", "-policy", p.Encode(), fmt.Sprintf("-lockdown=%v", lockdown))
	cmd.ExtraFiles = []*os.File{proxyEnd} // fd 3
	cmd.Stderr = os.Stderr
	cmd.Env = []string{} // nothing from ksar's environment is needed
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	out, err := cmd.StdoutPipe()
	if err != nil {
		vmEnd.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		vmEnd.Close()
		return nil, fmt.Errorf("start network proxy: %w", err)
	}
	ready := make(chan bool, 1)
	go func() {
		sc := bufio.NewScanner(out)
		first := true
		for sc.Scan() {
			var e netproxy.Event
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if first {
				first = false
				ready <- e.Kind == "ready"
				continue
			}
			onEvent(e)
		}
		if first {
			ready <- false
		}
		// No cmd.Wait: by now ksar is locked down and may not call
		// waitid. The exited proxy is reaped when ksar itself exits.
	}()
	select {
	case ok := <-ready:
		if !ok {
			vmEnd.Close()
			return nil, errors.New("network proxy failed to start")
		}
	case <-time.After(5 * time.Second):
		vmEnd.Close()
		cmd.Process.Kill()
		return nil, errors.New("network proxy did not start in time")
	}
	return &vmm.Net{Backend: vmEnd, MAC: netproxy.GuestMAC}, nil
}
