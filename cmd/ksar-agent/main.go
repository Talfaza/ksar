// Command ksar-agent runs as PID 1 inside a Ksar microVM. It prepares the
// guest system, then serves the host: receiving files, running commands
// and streaming their output back.
//
// It reads its instructions from the kernel command line:
//
//	ksar.root=/dev/vda   mount this read-only image (EROFS) under a RAM
//	                     overlay and switch into it
//	ksar.layers=/dev/vdb,/dev/vdc
//	                     extra read-only layers stacked on the image,
//	                     topmost first (e.g. an installed desktop)
//	ksar.vsock=PORT      talk to the host over vsock (CID 2, PORT); without
//	                     it, fall back to the serial port /dev/ttyS1
//
// It must be a static binary: CGO_ENABLED=0 go build ./cmd/ksar-agent
package main

import (
	"archive/tar"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
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
		var layers []string
		if l := args["ksar.layers"]; l != "" {
			layers = strings.Split(l, ",")
		}
		if err := switchRoot(root, args["ksar.rootfstype"], layers); err != nil {
			fatal("switch to root %s: %v", root, err)
		}
	}
	setupSystem()
	if args["ksar.display"] == "1" {
		setupDisplay()
	}

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
// upper is the overlay's writable layer, opened before the root switch
// hides it: Export reads the guest's changes from it.
var upper *os.Root

func switchRoot(dev, fstype string, layers []string) error {
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
	lowers := []string{"/.ksar/lower"}
	for i := len(layers) - 1; i >= 0; i-- { // the last listed is just above the image
		d := fmt.Sprintf("/.ksar/layer%d", i)
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
		if err := syscall.Mount(layers[i], d, fstype, syscall.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("mount layer %s: %w", layers[i], err)
		}
		lowers = append([]string{d}, lowers...)
	}
	// Writes live in RAM; allow most of it (tmpfs defaults to half), so
	// installing software in the guest isn't cut short.
	if err := syscall.Mount("tmpfs", "/.ksar/rw", "tmpfs", 0, "mode=0755,size=85%"); err != nil {
		return fmt.Errorf("mount writable layer: %w", err)
	}
	for _, d := range []string{"/.ksar/rw/upper", "/.ksar/rw/work"} {
		if err := os.Mkdir(d, 0o755); err != nil {
			return err
		}
	}
	if err := syscall.Mount("overlay", "/.ksar/root", "overlay", 0,
		"lowerdir="+strings.Join(lowers, ":")+",upperdir=/.ksar/rw/upper,workdir=/.ksar/rw/work"); err != nil {
		return fmt.Errorf("mount overlay: %w", err)
	}
	var err error
	if upper, err = os.OpenRoot("/.ksar/rw/upper"); err != nil {
		return err
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
	setupHosts()
	setupDNS()
}

// setupHosts makes localhost and the guest's own name resolve locally.
// Images exported from Docker have an empty /etc/hosts (Docker fills it
// in when a container starts), and without these lines a lookup of
// localhost goes to the network, which a run without one doesn't have.
func setupHosts() {
	old, _ := os.ReadFile("/etc/hosts")
	if bytes.Contains(old, []byte("localhost")) {
		return
	}
	os.MkdirAll("/etc", 0o755)
	os.Remove("/etc/hosts") // may be a symlink
	if err := os.WriteFile("/etc/hosts", append([]byte("127.0.0.1\tlocalhost ksar\n::1\tlocalhost ip6-localhost\n"), old...), 0o644); err != nil {
		logf("hosts: %v", err)
	}
}

// setupDisplay prepares for X on the run's screen. The guest has no udev,
// so Xorg can't discover the virtio keyboard and tablet by itself: an
// xorg.conf.d snippet names their event devices. Programs default to
// DISPLAY=:0.
// notes are reported to the host in the Ready message.
var notes []string

func setupDisplay() {
	defaultEnv = append(defaultEnv, "DISPLAY=:0")
	events := map[string]string{} // device name -> /dev/input/eventN
	paths, _ := filepath.Glob("/sys/class/input/event*/device/name")
	for _, p := range paths {
		name, err := os.ReadFile(p)
		if err == nil {
			events[strings.TrimSpace(string(name))] = "/dev/input/" + filepath.Base(filepath.Dir(filepath.Dir(p)))
		}
	}
	kbd, tablet := events["Ksar Keyboard"], events["Ksar Tablet"]
	if kbd == "" || tablet == "" {
		logf("display: input devices not found")
		return
	}
	// libinput only accepts devices udev has initialized, and the guest
	// has no udev: write the database entries udev would have.
	udevInput(kbd, "E:ID_INPUT_KEY=1\nE:ID_INPUT_KEYBOARD=1\n")
	udevInput(tablet, "E:ID_INPUT_MOUSE=1\n")
	input := xorgModule("input", "evdev", "libinput")
	video := xorgModule("drivers", "modesetting", "fbdev")
	if input == "" && video == "" {
		notes = append(notes, "this image has no X server (Xorg), so nothing can draw on the display; "+
			"use an image with a desktop, e.g. examples/desktop")
		return
	}
	if input == "" || video == "" {
		notes = append(notes, "this image has Xorg but lacks an input driver (xf86-input-evdev or -libinput) "+
			"or a video driver (modesetting or fbdev), so X can't use the display")
		return
	}
	devOpt := `Option "kmsdev" "/dev/dri/card0"`
	if video == "fbdev" {
		devOpt = `Option "fbdev" "/dev/fb0"`
	}
	conf := fmt.Sprintf(`# Written by ksar-agent for this run's virtual screen.
Section "ServerFlags"
	Option "AutoAddDevices" "false"
	Option "AutoEnableDevices" "false"
EndSection
Section "Device"
	Identifier "ksar-screen"
	Driver "%s"
	%s
EndSection
Section "InputDevice"
	Identifier "ksar-keyboard"
	Driver "%s"
	Option "Device" "%s"
EndSection
Section "InputDevice"
	Identifier "ksar-tablet"
	Driver "%s"
	Option "Device" "%s"
EndSection
Section "ServerLayout"
	Identifier "ksar"
	Screen "ksar-screen-0"
	InputDevice "ksar-keyboard" "CoreKeyboard"
	InputDevice "ksar-tablet" "CorePointer"
EndSection
Section "Screen"
	Identifier "ksar-screen-0"
	Device "ksar-screen"
EndSection
`, video, devOpt, input, kbd, input, tablet)
	os.MkdirAll("/etc/X11/xorg.conf.d", 0o755)
	if err := os.WriteFile("/etc/X11/xorg.conf.d/10-ksar.conf", []byte(conf), 0o644); err != nil {
		logf("display: %v", err)
	}
}

// udevInput writes /run/udev/data/cMAJ:MIN for an input event device.
func udevInput(dev, props string) {
	b, err := os.ReadFile("/sys/class/input/" + filepath.Base(dev) + "/dev")
	if err != nil {
		return
	}
	os.MkdirAll("/run/udev/data", 0o755)
	data := fmt.Sprintf("I:%d\nE:ID_INPUT=1\n%sG:seat\nQ:seat\n", time.Now().UnixMicro(), props)
	os.WriteFile("/run/udev/data/c"+strings.TrimSpace(string(b)), []byte(data), 0o644)
}

// xorgModule returns the first of the named Xorg driver modules installed.
func xorgModule(kind string, names ...string) string {
	for _, n := range names {
		for _, dir := range []string{"/usr/lib/xorg/modules", "/usr/lib64/xorg/modules", "/usr/lib/x86_64-linux-gnu/xorg/modules"} {
			if _, err := os.Stat(filepath.Join(dir, kind, n+"_drv.so")); err == nil {
				return n
			}
		}
	}
	return ""
}

// setupDNS points the resolver at Ksar's network proxy when the run has a
// network: the kernel configured eth0 from the ip= parameter and lists the
// DNS server in /proc/net/pnp, which is already in resolv.conf format.
func setupDNS() {
	pnp, err := os.ReadFile("/proc/net/pnp")
	if err != nil || !bytes.Contains(pnp, []byte("nameserver")) {
		return // no network for this run
	}
	os.MkdirAll("/etc", 0o755)
	os.Remove("/etc/resolv.conf") // often a symlink into /run in images
	if err := os.WriteFile("/etc/resolv.conf", pnp, 0o644); err != nil {
		logf("resolv.conf: %v", err)
	}
}

type agent struct {
	in  io.Reader
	out *proto.Writer

	file  *os.File       // file being received, if any
	fileE error          // first error during the current transfer
	stdin *io.PipeWriter // running command's stdin, if any
	busy  atomic.Bool    // a command is running

	tarW    *io.PipeWriter // directory copy in progress, if any
	tarDone chan proto.Result
}

func (a *agent) serve() error {
	var u syscall.Utsname
	syscall.Uname(&u)
	if err := a.out.WriteJSON(proto.TypeReady, proto.Ready{
		Version: proto.Version, Kernel: cstr(u.Release[:]), Notes: notes,
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
	case proto.TypeTarOpen:
		var m proto.TarOpen
		if err := proto.Decode(f, &m); err != nil {
			return err
		}
		a.openTar(m.Dir)
	case proto.TypeTarData:
		if a.tarW != nil {
			// Blocks until the extractor consumes it; credit is granted
			// only after, so the host never runs ahead of the disk.
			a.tarW.Write(f.Payload)
		}
	case proto.TypeHello:
		var m proto.Hello
		if err := proto.Decode(f, &m); err != nil {
			return err
		}
		hello(m)
	case proto.TypeExport:
		var m proto.Export
		if err := proto.Decode(f, &m); err != nil {
			return err
		}
		return a.export(m)
	case proto.TypeTarClose:
		res := proto.Result{Error: "no directory copy in progress"}
		if a.tarW != nil {
			a.tarW.Close()
			res = <-a.tarDone
			a.tarW, a.tarDone = nil, nil
		}
		return a.out.WriteJSON(proto.TypeResult, res)
	default:
		return fmt.Errorf("unknown frame type %#x", f.Type)
	}
	return nil
}

// openTar starts extracting a tar stream into dir. On any error the
// extractor keeps draining the stream so the protocol stays in step, and
// reports the first error when the host closes the stream.
func (a *agent) openTar(dir string) {
	if a.tarW != nil {
		a.tarW.CloseWithError(errors.New("superseded"))
		<-a.tarDone
	}
	pr, pw := io.Pipe()
	done := make(chan proto.Result, 1)
	a.tarW, a.tarDone = pw, done
	go func() {
		var res proto.Result
		if !filepath.IsAbs(dir) {
			res.Error = fmt.Sprintf("destination %q is not absolute", dir)
		} else if n, err := extractTar(pr, dir); err != nil {
			res.Error, res.Files = err.Error(), n
		} else {
			res.Files = n
		}
		io.Copy(io.Discard, pr)
		done <- res
	}()
}

// extractTar unpacks a tar stream under dest. Every name is confined to
// dest (absolute names and ".." are folded back inside), regular files
// are created without following an existing symlink, and anything other
// than files, directories and symlinks is ignored.
func extractTar(r io.Reader, dest string) (int, error) {
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return 0, err
	}
	tr := tar.NewReader(r)
	n := 0
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return n, nil
		}
		if err != nil {
			return n, fmt.Errorf("read archive: %w", err)
		}
		rel := path.Clean("/" + h.Name)
		if rel == "/" {
			continue
		}
		target := filepath.Join(dest, filepath.FromSlash(rel))
		mode := os.FileMode(h.Mode & 0o7777)
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return n, err
			}
			os.Chmod(target, mode|0o700) // keep it writable for the files inside
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return n, err
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
			if err != nil {
				return n, err
			}
			_, err = io.Copy(f, tr)
			if err == nil {
				err = f.Chmod(mode)
			}
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return n, fmt.Errorf("%s: %w", rel, err)
			}
			os.Chtimes(target, h.ModTime, h.ModTime)
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return n, err
			}
			os.Remove(target)
			if err := os.Symlink(h.Linkname, target); err != nil {
				return n, err
			}
		default:
			continue
		}
		n++
	}
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
