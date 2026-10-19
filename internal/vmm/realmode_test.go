package vmm

import (
	"bytes"
	"errors"
	"os"
	"testing"
	"time"

	"ksar/internal/guest"
)

func requireKVM(t *testing.T) {
	t.Helper()
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		t.Skipf("KVM not available: %v", err)
	}
	f.Close()
}

func TestHello(t *testing.T) {
	requireKVM(t)
	var out bytes.Buffer
	res, err := RunRealMode(RealModeConfig{Code: guest.Hello, Timeout: 5 * time.Second, Output: &out})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := out.String(), "Hello from inside a Ksar microVM!\n"; got != want {
		t.Errorf("output = %q, want %q", got, want)
	}
	if res.ExitCode != guest.HelloExitCode {
		t.Errorf("exit code = %d, want %d", res.ExitCode, guest.HelloExitCode)
	}
}

func TestTimeoutStopsSpinningGuest(t *testing.T) {
	requireKVM(t)
	start := time.Now()
	_, err := RunRealMode(RealModeConfig{Code: guest.Spin, Timeout: 200 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("err = %v, want ErrTimeout", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("timeout took %v, want about 200ms", d)
	}
}

func TestOutputIsCapped(t *testing.T) {
	requireKVM(t)
	var out bytes.Buffer
	res, err := RunRealMode(RealModeConfig{Code: guest.Hello, Output: &out, MaxOutput: 5, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if out.String() != "Hello" || !res.Truncated {
		t.Errorf("output = %q truncated=%v, want %q truncated=true", out.String(), res.Truncated, "Hello")
	}
}
