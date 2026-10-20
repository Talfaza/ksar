package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// scopeEnv marks a ksar process already running in its cgroup scope.
const scopeEnv = "KSAR_SCOPE"

// cgroupLimits are the resource limits for one ksar run.
type cgroupLimits struct {
	MemoryMax int64 // bytes
	TasksMax  int
	CPUQuota  int // percent of one CPU
}

// enterScope re-executes ksar inside a transient systemd user scope with
// the given limits: the VM process can't use more memory than the guest
// needs plus headroom, never swaps guest memory to disk, and can't create
// unbounded threads or hog every CPU. It only returns on failure or when
// no scope is needed; on success the process is replaced.
//
// mode is "auto" (use systemd if reachable, otherwise continue without),
// "on" (fail if it isn't), or "off".
func enterScope(mode string, limits cgroupLimits, verbose bool) error {
	if mode == "off" || os.Getenv(scopeEnv) != "" {
		return nil
	}
	if mode != "auto" && mode != "on" {
		return fmt.Errorf("-cgroup %q: want auto, on or off", mode)
	}
	run, err := exec.LookPath("systemd-run")
	if err == nil && !userManagerReachable() {
		err = errors.New("no systemd user manager for this session")
	}
	if err != nil {
		if mode == "on" {
			return fmt.Errorf("-cgroup on: %w", err)
		}
		if verbose {
			fmt.Fprintf(os.Stderr, "[ksar] running without cgroup limits: %v\n", err)
		}
		return nil
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	var id [4]byte
	rand.Read(id[:])
	unit := fmt.Sprintf("ksar-%d-%s.scope", os.Getpid(), hex.EncodeToString(id[:]))

	args := []string{run, "--user", "--scope", "--quiet", "--collect", "--unit=" + unit,
		fmt.Sprintf("--property=MemoryMax=%d", limits.MemoryMax),
		"--property=MemorySwapMax=0", // guest memory never goes to disk
		fmt.Sprintf("--property=TasksMax=%d", limits.TasksMax),
		fmt.Sprintf("--property=CPUQuota=%d%%", limits.CPUQuota),
		"--", self}
	args = append(args, os.Args[1:]...)
	env := append(os.Environ(), scopeEnv+"="+unit)
	// Replace this process: exit status, signals and stdio pass straight
	// through, and there is no extra parent process.
	return syscall.Exec(run, args, env)
}

// userManagerReachable checks for the systemd user manager's socket
// without connecting, so the common "no user session" case (ssh without
// lingering, containers) is detected without a slow failure.
func userManagerReachable() bool {
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		return false
	}
	for _, p := range []string{filepath.Join(dir, "systemd", "private"), filepath.Join(dir, "bus")} {
		if st, err := os.Stat(p); err == nil && st.Mode()&os.ModeSocket != 0 {
			return true
		}
	}
	return false
}

// currentScope describes the cgroup this process runs in, for -v output.
func currentScope() string {
	unit := os.Getenv(scopeEnv)
	if unit == "" {
		return ""
	}
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil || !strings.Contains(string(b), unit) {
		return unit + " (not confirmed)"
	}
	return unit
}
