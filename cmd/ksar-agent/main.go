// Command ksar-agent runs as PID 1 inside a Ksar microVM. It prepares the
// guest system, then serves the host: receiving files, running commands
// and streaming their output back.
//
// It reads its instructions from the kernel command line:
//
//	ksar.root=/dev/vda   mount this read-only image (EROFS) under a RAM
//	                     overlay and switch into it
//	ksar.vsock=PORT      talk to the host over vsock (CID 2, PORT); without
//	                     it, fall back to the serial port /dev/ttyS1
//
// It must be a static binary: CGO_ENABLED=0 go build ./cmd/ksar-agent
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"ksar/internal/agent/proto"
)

const channelDevice = "/dev/ttyS1"

// crtscts is CRTSCTS from <asm-generic/termbits.h> (missing from package syscall).
const crtscts = 0x80000000

var defaultEnv = []string{
	"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
	"HOME=/root",
	"TERM=dumb",
	"LANG=C.UTF-8",
}

func main() {
	if os.Getpid() != 1 {
		fmt.Fprintln(os.Stderr, "ksar-agent: must run as PID 1 inside a Ksar VM")
		os.Exit(1)
	}
	logf("starting")
	// The kernel starts PID 1 with almost no environment. exec.Command
	// resolves program names using our own PATH, so set it here.
	for _, kv := range defaultEnv {
		k, v, _ := strings.Cut(kv, "=")
		os.Setenv(k, v)
	}
	args := kernelArgs()
	if root := args["ksar.root"]; root != "" {
		if err := switchRoot(root, args["ksar.rootfstype"]); err != nil {
			fatal("switch to root %s: %v", root, err)
		}
	}
	setupSystem()

	conn, err := openChannel(args)
	if err != nil {
		fatal("%v", err)
	}
	a := &agent{in: conn, out: proto.NewWriter(conn)}
	if err := a.serve(); err != nil {
		fatal("%v", err)
	}
	fatal("host channel closed")
}

// kernelArgs parses key=value words from /proc/cmdline.
func kernelArgs() map[string]string {
	os.MkdirAll("/proc", 0o555)
	syscall.Mount("proc", "/proc", "proc", 0, "") // may already be mounted
	b, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		fatal("read /proc/cmdline: %v", err)
	}
	args := map[string]string{}
	for _, w := range strings.Fields(string(b)) {
		k, v, _ := strings.Cut(w, "=")
		args[k] = v
	}
	return args
}

// switchRoot mounts the image read-only, stacks a RAM-backed writable
// layer on it with overlayfs, and makes that the root. Writes never reach
// the image, so one image file serves many runs, each starting clean.
func switchRoot(dev, fstype string) error {
	if fstype == "" {
		fstype = "erofs"
	}
	os.MkdirAll("/dev", 0o755)
	if err := syscall.Mount("devtmpfs", "/dev", "devtmpfs", syscall.MS_NOSUID, "mode=0755"); err != nil &&
		!errors.Is(err, syscall.EBUSY) {
		return fmt.Errorf("mount /dev: %w", err)
	}
	for _, d := range []string{"/.ksar/lower", "/.ksar/rw", "/.ksar/root"} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	if err := syscall.Mount(dev, "/.ksar/lower", fstype, syscall.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("mount %s (%s): %w", dev, fstype, err)
	}
	if err := syscall.Mount("tmpfs", "/.ksar/rw", "tmpfs", 0, "mode=0755"); err != nil {
		return fmt.Errorf("mount writable layer: %w", err)
	}
	for _, d := range []string{"/.ksar/rw/upper", "/.ksar/rw/work"} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return err
		}
	}
	if err := syscall.Mount("overlay", "/.ksar/root", "overlay", 0,
		"lowerdir=/.ksar/lower,upperdir=/.ksar/rw/upper,workdir=/.ksar/rw/work"); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	// Detach the initramfs's /proc and /dev; setupSystem mounts fresh ones
	// inside the new root.
	syscall.Unmount("/proc", syscall.MNT_DETACH)
	syscall.Unmount("/dev", syscall.MNT_DETACH)

	// Same dance as switch_root(8): move the new root over / and chroot.
	if err := os.Chdir("/.ksar/root"); err != nil {
		return err
	}
	if err := syscall.Mount(".", "/", "", syscall.MS_MOVE, ""); err != nil {
		return fmt.Errorf("move root: %w", err)
	}
	if err := syscall.Chroot("."); err != nil {
		return fmt.Errorf("chroot: %w", err)
	}
	return os.Chdir("/")
}

// openChannel connects to the host over vsock if the host offered it,
// otherwise opens the serial fallback.
func openChannel(args map[string]string) (io.ReadWriter, error) {
	if p := args["ksar.vsock"]; p != "" {
		port, err := strconv.ParseUint(p, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bad ksar.vsock=%q", p)
		}
		return dialHost(uint32(port))
	}
	tty, err := os.OpenFile(channelDevice, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", channelDevice, err)
	}
	if err := makeRaw(int(tty.Fd())); err != nil {
		return nil, fmt.Errorf("raw mode on %s: %w", channelDevice, err)
	}
	return tty, nil
}

// dialHost connects to the host (vsock CID 2) on port, retrying briefly
// in case the virtio driver is still coming up.
func dialHost(port uint32) (*os.File, error) {
	const afVsock, cidHost = 40, 2
	var sa [16]byte
	sa[0] = afVsock
	binary.LittleEndian.PutUint32(sa[4:], port)
	binary.LittleEndian.PutUint32(sa[8:], cidHost)
	var lastErr error
	for range 200 {
		fd, err := syscall.Socket(afVsock, syscall.SOCK_STREAM|syscall.SOCK_CLOEXEC, 0)
		if err != nil {
			return nil, fmt.Errorf("vsock socket: %w", err)
		}
		_, _, e := syscall.Syscall(syscall.SYS_CONNECT, uintptr(fd), uintptr(unsafe.Pointer(&sa)), uintptr(len(sa)))
		if e == 0 {
			return os.NewFile(uintptr(fd), "vsock-host"), nil
		}
		syscall.Close(fd)
		lastErr = e
		time.Sleep(10 * time.Millisecond)
	}
	return nil, fmt.Errorf("connect to host vsock port %d: %w", port, lastErr)
}

// setupSystem mounts the usual pseudo filesystems and brings up loopback
// networking. Failures are logged, not fatal: a command may still work.
func setupSystem() {
	for _, m := range []struct{ src, dst, fstype, data string }{
		{"proc", "/proc", "proc", ""},
		{"sysfs", "/sys", "sysfs", ""},
		{"devtmpfs", "/dev", "devtmpfs", "mode=0755"},
		{"devpts", "/dev/pts", "devpts", "mode=0620,ptmxmode=0666"},
		{"tmpfs", "/dev/shm", "tmpfs", "mode=1777"},
		{"tmpfs", "/run", "tmpfs", "mode=0755"},
	} {
		os.MkdirAll(m.dst, 0o755)
		if err := syscall.Mount(m.src, m.dst, m.fstype, syscall.MS_NOSUID, m.data); err != nil &&
			!errors.Is(err, syscall.EBUSY) {
			logf("mount %s: %v", m.dst, err)
		}
	}
	os.MkdirAll("/tmp", 0o1777)
	os.Chmod("/tmp", 0o1777)
	os.Symlink("/proc/self/fd", "/dev/fd")
	syscall.Sethostname([]byte("ksar"))
	if err := loopbackUp(); err != nil {
		logf("loopback: %v", err)
	}
}

type agent struct {
	in  io.Reader
	out *proto.Writer

	file  *os.File       // file being received, if any
	fileE error          // first error during the current transfer
	stdin *io.PipeWriter // running command's stdin, if any
	busy  atomic.Bool    // a command is running
}

func (a *agent) serve() error {
	var u syscall.Utsname
	syscall.Uname(&u)
	if err := a.out.WriteJSON(proto.TypeReady, proto.Ready{
		Version: proto.Version, Kernel: cstr(u.Release[:]),
	}); err != nil {
		return err
	}
	logf("ready")
	for {
		f, err := proto.ReadFrame(a.in, proto.MaxGuestFrame)
		if err != nil {
			return fmt.Errorf("read from host: %w", err)
		}
		if err := a.handle(f); err != nil {
			return err
		}
		// Grant back exactly what was consumed.
		if err := a.out.Credit(proto.FrameSize(f)); err != nil {
			return err
		}
	}
}

func (a *agent) handle(f proto.Frame) error {
	switch f.Type {
	case proto.TypeFileOpen:
		var m proto.FileOpen
		if err := proto.Decode(f, &m); err != nil {
			return err
		}
		a.openFile(m)
	case proto.TypeFileData:
		if a.file != nil && a.fileE == nil {
			_, a.fileE = a.file.Write(f.Payload)
		}
	case proto.TypeFileClose:
		res := proto.Result{}
		if a.file == nil && a.fileE == nil {
			a.fileE = errors.New("no file open")
		}
		if a.file != nil {
			if err := a.file.Close(); a.fileE == nil {
				a.fileE = err
			}
		}
		if a.fileE != nil {
			res.Error = a.fileE.Error()
		}
		a.file, a.fileE = nil, nil
		return a.out.WriteJSON(proto.TypeResult, res)
	case proto.TypeExec:
		var m proto.Exec
		if err := proto.Decode(f, &m); err != nil {
			return err
		}
		return a.exec(m)
	case proto.TypeStdin:
		if a.stdin != nil {
			// The pipe blocks until the command reads; credit is only
			// granted after, so the host slows down to the command's pace.
			a.stdin.Write(f.Payload)
		}
	case proto.TypeStdinClose:
		if a.stdin != nil {
			a.stdin.Close()
			a.stdin = nil
		}
	default:
		return fmt.Errorf("unknown frame type %#x", f.Type)
	}
	return nil
}

func (a *agent) openFile(m proto.FileOpen) {
	if a.file != nil {
		a.file.Close()
	}
	a.file, a.fileE = nil, nil
	if !filepath.IsAbs(m.Path) {
		a.fileE = fmt.Errorf("path %q is not absolute", m.Path)
		return
	}
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o755); err != nil {
		a.fileE = err
		return
	}
	mode := os.FileMode(m.Mode & 0o7777)
	if mode == 0 {
		mode = 0o644
	}
	a.file, a.fileE = os.OpenFile(m.Path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if a.fileE == nil {
		a.fileE = a.file.Chmod(mode) // OpenFile's mode is filtered by umask
	}
}

func (a *agent) exec(m proto.Exec) error {
	if a.busy.Load() {
		return a.out.WriteJSON(proto.TypeExit, proto.Exit{Code: 125, Error: "a command is already running"})
	}
	if len(m.Args) == 0 {
		return a.out.WriteJSON(proto.TypeExit, proto.Exit{Code: 125, Error: "empty command"})
	}
	env := append(append([]string(nil), defaultEnv...), m.Env...)
	// exec.Command would look the program up in the agent's own PATH; use
	// the PATH the command will actually run with (image or -e), so e.g.
	// a Go image's /usr/local/go/bin is found.
	prog, err := lookPath(m.Args[0], envValue(env, "PATH"))
	if err != nil {
		return a.out.WriteJSON(proto.TypeExit, proto.Exit{Code: 127, Error: err.Error()})
	}
	cmd := exec.Command(prog, m.Args[1:]...)
	cmd.Args[0] = m.Args[0]
	cmd.Env = env
	cmd.Dir = m.Dir
	if cmd.Dir == "" {
		cmd.Dir = "/"
	}
	cmd.Stdout = &streamWriter{w: a.out, typ: proto.TypeStdout}
	cmd.Stderr = &streamWriter{w: a.out, typ: proto.TypeStderr}
	pr, pw := io.Pipe()
	cmd.Stdin = pr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		pr.Close()
		code := 126
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			code = 127
		}
		return a.out.WriteJSON(proto.TypeExit, proto.Exit{Code: code, Error: err.Error()})
	}
	a.busy.Store(true)
	a.stdin = pw
	go func() {
		err := cmd.Wait()
		pr.CloseWithError(io.ErrClosedPipe) // unblock pending stdin writes
		reapZombies()
		a.busy.Store(false)
		a.out.WriteJSON(proto.TypeExit, exitStatus(err))
	}()
	return nil
}

// envValue returns the last value of key in env (later entries win, as
// os/exec does when it deduplicates).
func envValue(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}

// lookPath finds an executable like the shell does, in the given PATH.
func lookPath(name, path string) (string, error) {
	if strings.Contains(name, "/") {
		return name, nil
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return p, nil
		}
	}
	return "", fmt.Errorf("exec: %q: executable file not found in $PATH (%s)", name, path)
}

func exitStatus(err error) proto.Exit {
	if err == nil {
		return proto.Exit{}
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return proto.Exit{Code: 128 + int(ws.Signal()), Signal: ws.Signal().String()}
		}
		return proto.Exit{Code: ee.ExitCode()}
	}
	return proto.Exit{Code: 125, Error: err.Error()}
}

// reapZombies collects orphaned processes that were re-parented to us.
func reapZombies() {
	for {
		var ws syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}

// streamWriter turns a command's output into Stdout/Stderr frames.
type streamWriter struct {
	w   *proto.Writer
	typ uint8
}

func (s *streamWriter) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		chunk := p[:min(len(p), proto.MaxGuestFrame)]
		if err := s.w.Write(s.typ, chunk); err != nil {
			return n - len(p), err
		}
		p = p[len(chunk):]
	}
	return n, nil
}

// makeRaw puts a tty in raw mode, like cfmakeraw(3): no echo, no line
// editing, no signal characters, no CR/LF translation, 8-bit clean.
func makeRaw(fd int) error {
	var t syscall.Termios
	if err := ioctl(fd, syscall.TCGETS, unsafe.Pointer(&t)); err != nil {
		return err
	}
	t.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP |
		syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON | syscall.IXOFF
	t.Oflag &^= syscall.OPOST
	t.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	t.Cflag &^= syscall.CSIZE | syscall.PARENB | crtscts
	t.Cflag |= syscall.CS8 | syscall.CREAD | syscall.CLOCAL
	t.Cc[syscall.VMIN] = 1
	t.Cc[syscall.VTIME] = 0
	return ioctl(fd, syscall.TCSETS, unsafe.Pointer(&t))
}

// loopbackUp sets IFF_UP on "lo" so programs can use 127.0.0.1.
func loopbackUp() error {
	fd, err := syscall.Socket(syscall.AF_INET, syscall.SOCK_DGRAM|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)
	var ifr struct {
		Name  [16]byte
		Flags uint16
		_     [22]byte
	}
	copy(ifr.Name[:], "lo")
	if err := ioctl(fd, syscall.SIOCGIFFLAGS, unsafe.Pointer(&ifr)); err != nil {
		return err
	}
	ifr.Flags |= syscall.IFF_UP | syscall.IFF_RUNNING
	return ioctl(fd, syscall.SIOCSIFFLAGS, unsafe.Pointer(&ifr))
}

func ioctl(fd int, req uintptr, p unsafe.Pointer) error {
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(p))
	if errno != 0 {
		return errno
	}
	return nil
}

func cstr(b []int8) string {
	var sb bytes.Buffer
	for _, c := range b {
		if c == 0 {
			break
		}
		sb.WriteByte(byte(c))
	}
	return sb.String()
}

// logf writes to the kernel console (COM1), which the host shows with -v.
func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ksar-agent: "+strings.TrimSuffix(format, "\n")+"\n", args...)
}

// fatal logs and stops the VM. PID 1 must never exit (the kernel would
// panic), so it reboots instead, which ends the VM.
func fatal(format string, args ...any) {
	logf("fatal: "+format, args...)
	syscall.Sync()
	syscall.Reboot(syscall.LINUX_REBOOT_CMD_RESTART)
	select {}
}
