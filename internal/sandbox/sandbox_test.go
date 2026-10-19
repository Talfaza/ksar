package sandbox

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"ksar/internal/agent/proto"
)

// Test fixtures, built or found once per test binary.
var (
	testKernel []byte
	testAgent  []byte
)

func setup(t *testing.T) {
	t.Helper()
	if f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0); err != nil {
		t.Skipf("KVM not available: %v", err)
	} else {
		f.Close()
	}
	if testKernel == nil {
		path := os.Getenv("KSAR_TEST_KERNEL")
		if path == "" {
			var u syscall.Utsname
			syscall.Uname(&u)
			var rel strings.Builder
			for _, c := range u.Release {
				if c == 0 {
					break
				}
				rel.WriteByte(byte(c))
			}
			path = "/boot/vmlinuz-" + rel.String()
		}
		k, err := os.ReadFile(path)
		if err != nil {
			t.Skipf("no test kernel (set KSAR_TEST_KERNEL): %v", err)
		}
		testKernel = k
	}
	if testAgent == nil {
		bin := filepath.Join(t.TempDir(), "ksar-agent")
		cmd := exec.Command("go", "build", "-o", bin, "ksar/cmd/ksar-agent")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build agent: %v\n%s", err, out)
		}
		b, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		testAgent = b
	}
}

// alpine opens the Alpine image built by `make alpine`, or skips.
func alpine(t *testing.T) *os.File {
	t.Helper()
	f, err := os.Open("../../build/alpine.tar")
	if err != nil {
		t.Skip("no Alpine image; run `make alpine`")
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func run(t *testing.T, opts Options) (Result, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	opts.Kernel, opts.Agent = testKernel, testAgent
	opts.Stdout, opts.Stderr = &stdout, &stderr
	if opts.Timeout == 0 {
		opts.Timeout = 60 * time.Second
	}
	res, err := Run(opts)
	if err != nil {
		t.Fatalf("Run: %v\nconsole:\n%s", err, res.ConsoleTail)
	}
	return res, stdout.String(), stderr.String()
}

// Without an image the only program in the guest is the agent itself.
// Running it again (not as PID 1) exercises exec, stderr and exit codes.
func TestAgentOnly(t *testing.T) {
	setup(t)
	res, _, stderr := run(t, Options{Exec: proto.Exec{Args: []string{"/.ksar/agent"}}})
	if res.Exit.Code != 1 || !strings.Contains(stderr, "must run as PID 1") {
		t.Errorf("exit=%+v stderr=%q", res.Exit, stderr)
	}
	t.Logf("boot %v, total %v", res.Boot, res.Total)
}

func TestCommandNotFound(t *testing.T) {
	setup(t)
	res, _, _ := run(t, Options{Exec: proto.Exec{Args: []string{"/nope"}}})
	if res.Exit.Code != 127 || res.Exit.Error == "" {
		t.Errorf("exit = %+v, want code 127 with an error", res.Exit)
	}
}

func TestAlpineShell(t *testing.T) {
	setup(t)
	res, stdout, stderr := run(t, Options{
		Image: alpine(t),
		Exec: proto.Exec{
			Args: []string{"sh", "-c", `echo "$GREETING from $(hostname) in $(pwd)"; echo oops >&2; exit 3`},
			Env:  []string{"GREETING=hi"},
			Dir:  "/tmp",
		},
	})
	if stdout != "hi from ksar in /tmp\n" {
		t.Errorf("stdout = %q", stdout)
	}
	if stderr != "oops\n" {
		t.Errorf("stderr = %q", stderr)
	}
	if res.Exit.Code != 3 {
		t.Errorf("exit code = %d, want 3", res.Exit.Code)
	}
}

func TestStdinAndFiles(t *testing.T) {
	setup(t)
	// Larger than the flow-control window, to exercise credit grants.
	payload := bytes.Repeat([]byte("0123456789abcdef"), 8<<10) // 128 KiB
	res, stdout, _ := run(t, Options{
		Image: alpine(t),
		Files: []File{
			{Path: "/work/data.bin", Mode: 0o600, Data: payload},
			{Path: "/work/run.sh", Mode: 0o755, Data: []byte("#!/bin/sh\nwc -c < data.bin; wc -c; stat -c %a data.bin\n")},
		},
		Exec:  proto.Exec{Args: []string{"./run.sh"}, Dir: "/work"},
		Stdin: bytes.NewReader(payload),
	})
	want := "131072\n131072\n600\n"
	if stdout != want || res.Exit.Code != 0 {
		t.Errorf("stdout = %q (exit %d), want %q", stdout, res.Exit.Code, want)
	}
}

func TestSignalExit(t *testing.T) {
	setup(t)
	res, _, _ := run(t, Options{Image: alpine(t), Exec: proto.Exec{Args: []string{"sh", "-c", "kill -9 $$"}}})
	if res.Exit.Code != 137 || res.Exit.Signal != "killed" {
		t.Errorf("exit = %+v, want 137/killed", res.Exit)
	}
}

func TestTimeoutKillsVM(t *testing.T) {
	setup(t)
	start := time.Now()
	_, err := Run(Options{
		Kernel: testKernel, Agent: testAgent, Image: alpine(t),
		Exec:    proto.Exec{Args: []string{"sleep", "60"}},
		Timeout: 2 * time.Second,
	})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Errorf("took %v to time out", d)
	}
}
