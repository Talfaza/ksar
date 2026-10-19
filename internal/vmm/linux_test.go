package vmm

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ksar/internal/initramfs"
)

// hostKernel returns the running kernel's bzImage, or skips the test.
func hostKernel(t *testing.T) []byte {
	t.Helper()
	var u syscall.Utsname
	syscall.Uname(&u)
	var rel strings.Builder
	for _, c := range u.Release {
		if c == 0 {
			break
		}
		rel.WriteByte(byte(c))
	}
	path := os.Getenv("KSAR_TEST_KERNEL")
	if path == "" {
		path = "/boot/vmlinuz-" + rel.String()
	}
	k, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("no test kernel (set KSAR_TEST_KERNEL): %v", err)
	}
	return k
}

// End to end: boot a real kernel with the hello init and check it reached
// userspace and rebooted cleanly.
func TestBootLinux(t *testing.T) {
	requireKVM(t)
	kernel := hostKernel(t)

	dir := t.TempDir()
	initBin := filepath.Join(dir, "init")
	build := exec.Command("go", "build", "-o", initBin, "ksar/cmd/ksar-hello-init")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build hello-init: %v\n%s", err, out)
	}
	initBytes, err := os.ReadFile(initBin)
	if err != nil {
		t.Fatal(err)
	}
	var rd bytes.Buffer
	if err := initramfs.Minimal(&rd, initBytes); err != nil {
		t.Fatal(err)
	}

	var console bytes.Buffer
	res, err := RunLinux(LinuxConfig{
		Kernel:  kernel,
		Initrd:  rd.Bytes(),
		Timeout: 30 * time.Second,
		Output:  &console,
	})
	if err != nil {
		t.Fatalf("RunLinux: %v\nconsole:\n%s", err, console.String())
	}
	for _, want := range []string{
		"Hello from Linux userspace inside Ksar!",
		"pid:     1",
		"Ksar Virtual CPU",
		"is a 16550A",
	} {
		if !strings.Contains(console.String(), want) {
			t.Errorf("console missing %q", want)
		}
	}
	if res.Reason != "guest rebooted" {
		t.Errorf("reason = %q, want guest rebooted", res.Reason)
	}
	t.Logf("booted to userspace and rebooted in %v", res.Duration)
}

// A kernel with no init panics; panic=-1 must turn that into a reboot so
// the VM ends instead of hanging until the timeout.
func TestBootWithoutInitEnds(t *testing.T) {
	requireKVM(t)
	kernel := hostKernel(t)
	res, err := RunLinux(LinuxConfig{Kernel: kernel, Timeout: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reason != "guest rebooted" {
		t.Errorf("reason = %q", res.Reason)
	}
}
