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
	"ksar/internal/image"
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

// mode is one way of running the sandbox.
type mode struct {
	name   string
	kernel []byte
	virtio bool
}

var ksarKernel []byte

// modes returns the modes to test: legacy (serial + in-RAM image, any
// kernel) and, when the Ksar kernel is built, virtio (vsock + EROFS disk).
func modes(t *testing.T) []mode {
	setup(t)
	ms := []mode{{name: "legacy", kernel: testKernel}}
	if ksarKernel == nil {
		ksarKernel, _ = os.ReadFile("../../build/bzImage")
	}
	if ksarKernel != nil {
		ms = append(ms, mode{name: "virtio", kernel: ksarKernel, virtio: true})
	} else {
		t.Log("Ksar kernel not built (make kernel): skipping virtio mode")
	}
	return ms
}

// each runs f once per mode as subtests.
func each(t *testing.T, f func(t *testing.T, m mode)) {
	for _, m := range modes(t) {
		t.Run(m.name, func(t *testing.T) { f(t, m) })
	}
}

const alpineTar = "../../build/alpine.tar"

// withAlpine sets the Alpine root filesystem for the mode, or skips.
func withAlpine(t *testing.T, m mode, opts *Options) {
	t.Helper()
	if _, err := os.Stat(alpineTar); err != nil {
		t.Skip("no Alpine image; run `make alpine`")
	}
	if m.virtio {
		t.Setenv("XDG_CACHE_HOME", t.TempDir())
		disk, err := image.FromTar(alpineTar)
		if err != nil {
			t.Fatal(err)
		}
		opts.RootDisk = disk
		return
	}
	f, err := os.Open(alpineTar)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	opts.Image = f
}

func run(t *testing.T, m mode, opts Options) (Result, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	opts.Kernel, opts.Agent, opts.Virtio = m.kernel, testAgent, m.virtio
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
	each(t, func(t *testing.T, m mode) {
		res, _, stderr := run(t, m, Options{Exec: proto.Exec{Args: []string{"/.ksar/agent"}}})
		if res.Exit.Code != 1 || !strings.Contains(stderr, "must run as PID 1") {
			t.Errorf("exit=%+v stderr=%q", res.Exit, stderr)
		}
		t.Logf("boot %v, total %v", res.Boot, res.Total)
	})
}

func TestCommandNotFound(t *testing.T) {
	each(t, func(t *testing.T, m mode) {
		res, _, _ := run(t, m, Options{Exec: proto.Exec{Args: []string{"/nope"}}})
		if res.Exit.Code != 127 || res.Exit.Error == "" {
			t.Errorf("exit = %+v, want code 127 with an error", res.Exit)
		}
	})
}

func TestAlpineShell(t *testing.T) {
	each(t, func(t *testing.T, m mode) {
		opts := Options{
			Exec: proto.Exec{
				Args: []string{"sh", "-c", `echo "$GREETING from $(hostname) in $(pwd)"; echo oops >&2; exit 3`},
				Env:  []string{"GREETING=hi"},
				Dir:  "/tmp",
			},
		}
		withAlpine(t, m, &opts)
		res, stdout, stderr := run(t, m, opts)
		if stdout != "hi from ksar in /tmp\n" {
			t.Errorf("stdout = %q", stdout)
		}
		if stderr != "oops\n" {
			t.Errorf("stderr = %q", stderr)
		}
		if res.Exit.Code != 3 {
			t.Errorf("exit code = %d, want 3", res.Exit.Code)
		}
	})
}

func TestStdinAndFiles(t *testing.T) {
	each(t, func(t *testing.T, m mode) {
		// Larger than the flow-control window, to exercise credit grants.
		payload := bytes.Repeat([]byte("0123456789abcdef"), 8<<10) // 128 KiB
		opts := Options{
			Files: []File{
				{Path: "/work/data.bin", Mode: 0o600, Data: payload},
				{Path: "/work/run.sh", Mode: 0o755, Data: []byte("#!/bin/sh\nwc -c < data.bin; wc -c; stat -c %a data.bin\n")},
			},
			Exec:  proto.Exec{Args: []string{"./run.sh"}, Dir: "/work"},
			Stdin: bytes.NewReader(payload),
		}
		withAlpine(t, m, &opts)
		res, stdout, _ := run(t, m, opts)
		want := "131072\n131072\n600\n"
		if stdout != want || res.Exit.Code != 0 {
			t.Errorf("stdout = %q (exit %d), want %q", stdout, res.Exit.Code, want)
		}
	})
}

func TestSignalExit(t *testing.T) {
	each(t, func(t *testing.T, m mode) {
		opts := Options{Exec: proto.Exec{Args: []string{"sh", "-c", "kill -9 $$"}}}
		withAlpine(t, m, &opts)
		res, _, _ := run(t, m, opts)
		if res.Exit.Code != 137 || res.Exit.Signal != "killed" {
			t.Errorf("exit = %+v, want 137/killed", res.Exit)
		}
	})
}

func TestTimeoutKillsVM(t *testing.T) {
	each(t, func(t *testing.T, m mode) {
		opts := Options{
			Kernel: m.kernel, Agent: testAgent, Virtio: m.virtio,
			Exec:    proto.Exec{Args: []string{"sleep", "60"}},
			Timeout: 2 * time.Second,
		}
		withAlpine(t, m, &opts)
		start := time.Now()
		_, err := Run(opts)
		if !errors.Is(err, ErrTimeout) {
			t.Fatalf("err = %v, want ErrTimeout", err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Errorf("took %v to time out", d)
		}
	})
}

// The image disk is read-only and every run gets a fresh RAM overlay:
// writes in one run must not be visible in the next.
func TestRunsAreIsolated(t *testing.T) {
	each(t, func(t *testing.T, m mode) {
		for i, script := range []string{
			"echo leaked > /etc/ksar-marker && rm /bin/ls && echo wrote",
			"cat /etc/ksar-marker 2>/dev/null || echo clean; test -e /bin/ls && echo ls-present",
		} {
			opts := Options{Exec: proto.Exec{Args: []string{"sh", "-c", script}}}
			withAlpine(t, m, &opts)
			_, stdout, _ := run(t, m, opts)
			want := []string{"wrote\n", "clean\nls-present\n"}[i]
			if stdout != want {
				t.Errorf("run %d: stdout = %q, want %q", i, stdout, want)
			}
		}
	})
}

// The agent is the only thing the guest can use to reach the host, and
// the guest must not be able to see it in its filesystem (virtio mode
// switches root away from the initramfs).
func TestVirtioHidesAgent(t *testing.T) {
	for _, m := range modes(t) {
		if !m.virtio {
			continue
		}
		opts := Options{Exec: proto.Exec{Args: []string{"sh", "-c", "test -e /.ksar/agent && echo visible || echo hidden"}}}
		withAlpine(t, m, &opts)
		_, stdout, _ := run(t, m, opts)
		if stdout != "hidden\n" {
			t.Errorf("agent binary %s in the guest root", strings.TrimSpace(stdout))
		}
	}
}

// A program found only through a custom PATH (as images like golang set):
// the agent must resolve it with the command's PATH, not its own.
func TestCustomPATH(t *testing.T) {
	each(t, func(t *testing.T, m mode) {
		opts := Options{
			Files: []File{{Path: "/opt/tools/bin/hello-tool", Mode: 0o755, Data: []byte("#!/bin/sh\necho found via PATH\n")}},
			Exec: proto.Exec{Args: []string{"hello-tool"},
				Env: []string{"PATH=/opt/tools/bin:/usr/bin:/bin"}},
		}
		withAlpine(t, m, &opts)
		res, stdout, _ := run(t, m, opts)
		if res.Exit.Code != 0 || stdout != "found via PATH\n" {
			t.Errorf("exit %+v stdout %q", res.Exit, stdout)
		}
	})
}

// A whole directory, with nesting, an empty dir, an executable script and
// a symlink that points at a host file (copied as a link, not followed).
func TestCopyDirectory(t *testing.T) {
	src := t.TempDir()
	os.MkdirAll(filepath.Join(src, "a/b"), 0o755)
	os.MkdirAll(filepath.Join(src, "empty"), 0o755)
	os.WriteFile(filepath.Join(src, "a/b/data.txt"), []byte("nested"), 0o644)
	os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\necho ran\n"), 0o755)
	secret := filepath.Join(t.TempDir(), "host-secret")
	os.WriteFile(secret, []byte("TOP SECRET"), 0o600)
	os.Symlink(secret, filepath.Join(src, "leak"))

	each(t, func(t *testing.T, m mode) {
		var tarball bytes.Buffer
		st, err := PackDir(&tarball, src)
		if err != nil {
			t.Fatal(err)
		}
		if st.Files != 2 || st.Symlinks != 1 {
			t.Errorf("pack stats = %+v", st)
		}
		opts := Options{
			Dirs: []Dir{{Path: "/work/p", Tar: &tarball}},
			Exec: proto.Exec{Args: []string{"sh", "-c",
				"cat a/b/data.txt; echo; ./run.sh; test -d empty && echo empty-ok; cat leak 2>/dev/null || echo leak-blocked"},
				Dir: "/work/p"},
		}
		withAlpine(t, m, &opts)
		_, stdout, stderr := run(t, m, opts)
		if want := "nested\nran\nempty-ok\nleak-blocked\n"; stdout != want {
			t.Errorf("stdout = %q, want %q (stderr %q)", stdout, want, stderr)
		}
		if strings.Contains(stdout, "TOP SECRET") {
			t.Fatal("a host file leaked into the guest through a symlink")
		}
	})
}
