package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"ksar/internal/registry"
)

// cmdPs lists the VMs on this machine, whoever started them.
func cmdPs(args []string) int {
	fs := flag.NewFlagSet("ps", flag.ExitOnError)
	all := fs.Bool("a", false, "also show VMs that exited recently")
	asJSON := fs.Bool("json", false, "print JSON")
	quiet := fs.Bool("q", false, "print only ids")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: ksar ps [-a] [-q] [-json]\n\nLists running VMs (from the CLI and the web UI).\n\nFlags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	list, err := registry.List(*all)
	if err != nil {
		fmt.Fprintf(os.Stderr, "ksar: %v\n", err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if list == nil {
			list = []registry.Entry{}
		}
		enc.Encode(list)
		return 0
	}
	if *quiet {
		for _, e := range list {
			fmt.Println(e.ID)
		}
		return 0
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tSTATUS\tUPTIME\tMEMORY\tIMAGE\tCOMMAND")
	for _, e := range list {
		status := e.Status
		if status == "exited" && e.ExitCode != nil {
			status = fmt.Sprintf("exited (%d)", *e.ExitCode)
		}
		mem := "-"
		if registry.Alive(e) {
			if n := vmMemory(e.PID); n > 0 {
				mem = humanBytes(n)
			}
		}
		img := "-"
		if e.Image != "" {
			img = strings.TrimSuffix(filepath.Base(e.Image), ".tar")
		}
		what := e.Label
		if what == "" {
			what = strings.Join(e.Command, " ")
		}
		if e.Display != "" {
			what += "  [display]"
		}
		if e.Net != "" && e.Net != "none" {
			what += "  [net: " + e.Net + "]"
		}
		up := "-"
		if e.Status != "exited" {
			up = uptime(e.Started)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", e.ID, status, up, mem, img, truncate(what, 60))
	}
	tw.Flush()
	return 0
}

// cmdStop stops VMs by id (or unique id prefix).
func cmdStop(args []string) int {
	fs := flag.NewFlagSet("stop", flag.ExitOnError)
	wait := fs.Duration("t", 5*time.Second, "time to wait before killing")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "Usage: ksar stop [-t 5s] ID...\n\nStops VMs; an id prefix is enough if it is unique.\n\nFlags:\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	code := 0
	for _, id := range fs.Args() {
		e, err := registry.Find(id)
		if err != nil {
			fmt.Fprintf(os.Stderr, "ksar: %v\n", err)
			code = 1
			continue
		}
		if err := stopVM(e, *wait); err != nil {
			fmt.Fprintf(os.Stderr, "ksar: stop %s: %v\n", e.ID, err)
			code = 1
			continue
		}
		fmt.Println(e.ID)
	}
	return code
}

// stopVM ends the VM's ksar process: the VM lives inside it, and its
// network proxy dies with it. SIGTERM first, SIGKILL if it lingers.
func stopVM(e registry.Entry, wait time.Duration) error {
	if err := syscall.Kill(e.PID, syscall.SIGTERM); err != nil {
		if err == syscall.ESRCH {
			return nil
		}
		return err
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if !registry.Alive(e) {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	if registry.Alive(e) { // still the same process: force it
		syscall.Kill(e.PID, syscall.SIGKILL)
	}
	return nil
}

// vmMemory is the memory the VM's cgroup uses (guest RAM and all), or
// the process's resident size without a cgroup scope.
func vmMemory(pid int) int64 {
	if b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup"); err == nil {
		if _, path, ok := strings.Cut(strings.TrimSpace(string(b)), "::"); ok && strings.Contains(path, "ksar-") {
			if v, err := os.ReadFile(filepath.Join("/sys/fs/cgroup", path, "memory.current")); err == nil {
				if n, err := strconv.ParseInt(strings.TrimSpace(string(v)), 10, 64); err == nil {
					return n
				}
			}
		}
	}
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "VmRSS:"); ok {
			kb, _ := strconv.ParseInt(strings.Fields(v)[0], 10, 64)
			return kb << 10
		}
	}
	return 0
}

func uptime(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

func truncate(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}
