package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// End-to-end tests of the real ksar binary, built the way `make` builds
// it. They need KVM, the Ksar kernel (make kernel) and the Alpine image
// (make alpine).

func buildKsar(t *testing.T) (ksar, agent string) {
	t.Helper()
	if _, err := os.Stat("/dev/kvm"); err != nil {
		t.Skip("no KVM")
	}
	for _, f := range []string{"../../build/bzImage", "../../build/alpine.tar"} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("%s missing (make kernel alpine)", f)
		}
	}
	dir := t.TempDir()
	ksar, agent = filepath.Join(dir, "ksar"), filepath.Join(dir, "ksar-agent")
	for _, b := range []struct{ out, pkg string }{{ksar, "ksar/cmd/ksar"}, {agent, "ksar/cmd/ksar-agent"}} {
		cmd := exec.Command("go", "build", "-o", b.out, b.pkg)
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", b.pkg, err, out)
		}
	}
	return ksar, agent
}

func ksarRun(ksar, agent string, args ...string) *exec.Cmd {
	full := append([]string{"run", "-kernel", "../../build/bzImage", "-agent", agent,
		"-image", "../../build/alpine.tar"}, args...)
	cmd := exec.Command(ksar, full...)
	cmd.Env = append(os.Environ(), "XDG_CACHE_HOME="+os.TempDir()+"/ksar-e2e-cache")
	return cmd
}

func TestExitCodePassthrough(t *testing.T) {
	ksar, agent := buildKsar(t)
	cmd := ksarRun(ksar, agent, "--", "sh", "-c", "echo out; exit 42")
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 42 {
		t.Fatalf("err = %v, want exit 42", err)
	}
	if string(out) != "out\n" {
		t.Errorf("stdout = %q", out)
	}
}

// While a VM runs, inspect ksar from another process: the attacker's
// view of the VMM. The lockdown must be in force on every thread, and the
// process must be closed to ptrace and /proc/PID/mem (guest RAM).
func TestLockdownActiveDuringRun(t *testing.T) {
	ksar, agent := buildKsar(t)
	cmd := ksarRun(ksar, agent, "-cgroup=off", "--", "sleep", "3")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Wait()
	pid := cmd.Process.Pid

	// Wait until the guest is running (lockdown happens before that).
	var status string
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
		if err != nil {
			t.Fatal(err)
		}
		status = string(b)
		if strings.Contains(status, "Seccomp:\t2") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{"Seccomp:\t2", "NoNewPrivs:\t1"} {
		if !strings.Contains(status, want) {
			t.Errorf("/proc/%d/status lacks %q", pid, want)
		}
	}
	// Every thread, not just the main one, must carry the filter (TSYNC).
	tasks, _ := os.ReadDir("/proc/" + strconv.Itoa(pid) + "/task")
	for _, task := range tasks {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/task/" + task.Name() + "/status")
		if err == nil && !strings.Contains(string(b), "Seccomp:\t2") {
			t.Errorf("thread %s runs without the seccomp filter", task.Name())
		}
	}
	if f, err := os.Open("/proc/" + strconv.Itoa(pid) + "/mem"); err == nil {
		f.Close()
		t.Error("/proc/PID/mem (guest RAM) is readable by other processes")
	}
	limits, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/limits")
	if !strings.Contains(string(limits), "Max core file size        0                    0") {
		t.Errorf("core dumps not disabled:\n%s", limits)
	}
}

// A cgo build can't be locked down safely (glibc thread creation), so it
// must refuse to run guests rather than die silently mid-run.
func TestRefusesCgoBuild(t *testing.T) {
	_, agent := buildKsar(t)
	bin := filepath.Join(t.TempDir(), "ksar-cgo")
	build := exec.Command("go", "build", "-o", bin, "ksar/cmd/ksar")
	build.Env = append(os.Environ(), "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Skipf("cgo build unavailable: %v\n%s", err, out)
	}
	out, err := ksarRun(bin, agent, "--", "true").CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != exitKsarError || !strings.Contains(string(out), "cgo") {
		t.Fatalf("cgo build: err=%v out=%s", err, out)
	}
}
