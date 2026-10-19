// Command ksar-hello-init is a minimal PID 1 for testing Ksar's Linux boot.
// It reports what it sees from inside the VM, then reboots the guest,
// which makes Ksar exit cleanly.
//
// Build it static: CGO_ENABLED=0 go build ./cmd/ksar-hello-init
package main

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"syscall"
)

func main() {
	mount("proc", "/proc", "proc")
	mount("sysfs", "/sys", "sysfs")

	var u syscall.Utsname
	syscall.Uname(&u)
	uptime, _ := os.ReadFile("/proc/uptime")

	fmt.Println()
	fmt.Println("==============================================")
	fmt.Println(" Hello from Linux userspace inside Ksar!")
	fmt.Println("==============================================")
	fmt.Printf(" pid:     %d\n", os.Getpid())
	fmt.Printf(" kernel:  %s %s\n", str(u.Sysname[:]), str(u.Release[:]))
	fmt.Printf(" cpu:     %s\n", field("/proc/cpuinfo", "model name"))
	fmt.Printf(" memory:  %s\n", field("/proc/meminfo", "MemTotal"))
	fmt.Printf(" uptime:  %ss since kernel start\n", strings.Fields(string(uptime) + " ?")[0])
	fmt.Println("==============================================")
	fmt.Println(" rebooting (this ends the VM)")

	syscall.Sync()
	if err := syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART); err != nil {
		fmt.Println("reboot failed:", err)
	}
	select {} // PID 1 must never exit
}

func mount(src, dst, fstype string) {
	if err := syscall.Mount(src, dst, fstype, 0, ""); err != nil {
		fmt.Printf("mount %s: %v\n", dst, err)
	}
}

func field(file, key string) string {
	f, err := os.Open(file)
	if err != nil {
		return "?"
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), ":")
		if ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return "?"
}

func str(b []int8) string {
	var sb strings.Builder
	for _, c := range b {
		if c == 0 {
			break
		}
		sb.WriteByte(byte(c))
	}
	return sb.String()
}
