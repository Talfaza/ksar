// Command ksar runs untrusted code inside KVM microVMs.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"ksar/internal/guest"
	"ksar/internal/vmm"
)

const usage = `ksar - run untrusted code in KVM microVMs

Usage:
  ksar hello [-timeout 5s] [-spin]   boot a tiny real-mode guest (stage 1)
  ksar help                          show this help
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
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
