// Package desktop gives display runs a desktop: if the chosen image has
// none, a minimal XFCE is installed into it once, inside a Ksar VM, and
// kept as a separate read-only layer stacked on the image at run time.
//
// The install runs the image's own package manager, i.e. the image's
// (untrusted) code, so it happens in the same sandbox as any run: a
// microVM whose network reaches only that distribution's package
// mirrors. Only the files it changed come back, as a tar, and they are
// used the way an image is: read-only, inside later VMs.
package desktop

import (
	"archive/tar"
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"ksar/internal/image"
)

// Recipe version: bump to rebuild every desktop layer.
const recipeVersion = "xfce1"

// Info is what an image contains, as far as a desktop is concerned.
type Info struct {
	Distro  string `json:"distro"`  // os-release ID, e.g. debian
	Like    string `json:"like"`    // os-release ID_LIKE
	Version string `json:"version"` // os-release VERSION_ID
	Name    string `json:"name"`    // os-release PRETTY_NAME
	HasX    bool   `json:"has_x"`   // an Xorg server with a usable video driver
	HasXfce bool   `json:"has_xfce"`
}

// Ready reports whether the image can show a desktop by itself.
func (i Info) Ready() bool { return i.HasX && i.HasXfce }

func cacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	d := filepath.Join(base, "ksar", "desktop")
	return d, os.MkdirAll(d, 0o700)
}

// Inspected returns what an earlier Inspect found, without reading the
// tar; ok is false if it hasn't been inspected yet.
func Inspected(tarPath string) (info Info, ok bool) {
	key, err := image.Key(tarPath)
	if err != nil {
		return info, false
	}
	dir, err := cacheDir()
	if err != nil {
		return info, false
	}
	b, err := os.ReadFile(filepath.Join(dir, key+".info.json"))
	return info, err == nil && json.Unmarshal(b, &info) == nil
}

// Inspect reads an image tar's os-release and looks for Xorg and XFCE.
// Results are cached per tar contents.
func Inspect(tarPath string) (Info, error) {
	var info Info
	key, err := image.Key(tarPath)
	if err != nil {
		return info, err
	}
	dir, err := cacheDir()
	if err != nil {
		return info, err
	}
	cached := filepath.Join(dir, key+".info.json")
	if b, err := os.ReadFile(cached); err == nil && json.Unmarshal(b, &info) == nil {
		return info, nil
	}
	f, err := os.Open(tarPath)
	if err != nil {
		return info, err
	}
	defer f.Close()
	info, err = inspect(f)
	if err != nil {
		return info, err
	}
	if b, err := json.Marshal(info); err == nil {
		os.WriteFile(cached, b, 0o600)
	}
	return info, nil
}

func inspect(r io.Reader) (Info, error) {
	var info Info
	tr := tar.NewReader(r) // skips file data by seeking when r is a file
	releases := map[string]string{}
	links := map[string]string{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return info, fmt.Errorf("read image: %w", err)
		}
		name := strings.TrimPrefix(path.Clean("/"+h.Name), "/")
		switch name {
		case "etc/os-release", "usr/lib/os-release":
			if h.Typeflag == tar.TypeSymlink {
				links[name] = path.Join(path.Dir(name), h.Linkname)
			} else if h.Size < 64<<10 {
				b, _ := io.ReadAll(tr)
				releases[name] = string(b)
			}
		}
		switch {
		case strings.HasSuffix(name, "xorg/modules/drivers/modesetting_drv.so"),
			strings.HasSuffix(name, "xorg/modules/drivers/fbdev_drv.so"):
			info.HasX = true
		case name == "usr/bin/startxfce4":
			info.HasXfce = true
		}
	}
	rel := releases["etc/os-release"]
	if rel == "" {
		rel = releases[strings.TrimPrefix(path.Clean("/"+links["etc/os-release"]), "/")]
	}
	if rel == "" {
		rel = releases["usr/lib/os-release"]
	}
	sc := bufio.NewScanner(strings.NewReader(rel))
	for sc.Scan() {
		k, v, ok := strings.Cut(sc.Text(), "=")
		if !ok {
			continue
		}
		v = strings.Trim(v, `"'`)
		switch k {
		case "ID":
			info.Distro = v
		case "ID_LIKE":
			info.Like = v
		case "VERSION_ID":
			info.Version = v
		case "PRETTY_NAME":
			info.Name = v
		}
	}
	return info, nil
}

// Recipe installs a minimal XFCE with a distribution's package manager.
type Recipe struct {
	Family string
	Script string   // run with sh -c in the guest
	Allow  []string // the package mirrors it needs (the VM reaches nothing else)
}

const aptScript = `set -e
export DEBIAN_FRONTEND=noninteractive
apt-get update
apt-get install -y --no-install-recommends xserver-xorg-core xserver-xorg-input-libinput \
  xfce4 xfce4-terminal dbus-x11 fonts-dejavu-core adwaita-icon-theme
apt-get clean
rm -rf /var/lib/apt/lists/*`

// RecipeFor picks the install recipe for an image.
func RecipeFor(info Info) (Recipe, error) {
	// The image's own distribution first, then what it derives from.
	// Fedora only by name: RHEL clones list it in ID_LIKE but use
	// different repositories.
	family := ""
	for _, c := range append([]string{info.Distro}, strings.Fields(info.Like)...) {
		if c == "alpine" || c == "debian" || c == "ubuntu" || c == "arch" || c == "fedora" && c == info.Distro {
			family = c
			break
		}
	}
	switch family {
	case "alpine":
		return Recipe{Family: family, Allow: []string{"dl-cdn.alpinelinux.org"}, Script: `set -e
apk add --no-cache xorg-server xf86-input-libinput xf86-video-fbdev mesa-dri-gallium \
  xfce4 xfce4-terminal dbus-x11 font-dejavu adwaita-icon-theme`}, nil
	case "debian":
		return Recipe{Family: family, Allow: []string{"deb.debian.org", "security.debian.org"}, Script: aptScript}, nil
	case "ubuntu":
		return Recipe{Family: family, Allow: []string{"archive.ubuntu.com", "security.ubuntu.com"}, Script: aptScript}, nil
	case "fedora":
		// Fedora's metalink sends dnf to any of hundreds of mirrors; point
		// it at the main one for the install, then put the repos back.
		// Releases past their end of life move to the archive server.
		return Recipe{Family: family, Allow: []string{"dl.fedoraproject.org", "archives.fedoraproject.org"}, Script: `set -e
. /etc/os-release
base=https://dl.fedoraproject.org/pub/fedora/linux
if ! curl -sfI "$base/releases/$VERSION_ID/Everything/x86_64/os/repodata/repomd.xml" >/dev/null; then
  base=https://archives.fedoraproject.org/pub/archive/fedora/linux
fi
cp -a /etc/yum.repos.d /tmp/ksar-repos
sed -i -e 's/^metalink=/#metalink=/' \
  -e "s|^#baseurl=http://download.example/pub/fedora/linux|baseurl=$base|" \
  /etc/yum.repos.d/*.repo
dnf install -y --setopt=install_weak_deps=False --disablerepo='*' --enablerepo=fedora --enablerepo=updates \
  xorg-x11-server-Xorg xorg-x11-drv-libinput \
  xfce4-session xfwm4 xfce4-panel xfdesktop xfce4-settings xfce4-terminal Thunar dbus-x11 \
  dejavu-sans-fonts adwaita-icon-theme
dnf clean all
rm -rf /etc/yum.repos.d && mv /tmp/ksar-repos /etc/yum.repos.d`}, nil
	case "arch":
		return Recipe{Family: family, Allow: []string{"geo.mirror.pkgbuild.com"}, Script: `set -e
cp /etc/pacman.d/mirrorlist /tmp/ksar-mirrorlist
echo 'Server = https://geo.mirror.pkgbuild.com/$repo/os/$arch' > /etc/pacman.d/mirrorlist
pacman -Sy --noconfirm archlinux-keyring
pacman -Su --noconfirm --needed xorg-server xf86-input-libinput xfce4 ttf-dejavu
gpgconf --kill all 2>/dev/null || true
rm -rf /var/cache/pacman/pkg/*
mv /tmp/ksar-mirrorlist /etc/pacman.d/mirrorlist`}, nil
	}
	name := info.Name
	if name == "" {
		name = "this image's distribution"
	}
	return Recipe{}, fmt.Errorf("Ksar doesn't know how to install a desktop on %s yet "+
		"(supported: Alpine, Debian, Ubuntu, Fedora, Arch); use an image that has Xorg and XFCE", name)
}

// LayerPath is where the desktop layer for an image is cached.
func LayerPath(tarPath string) (string, error) {
	key, err := image.Key(tarPath)
	if err != nil {
		return "", err
	}
	dir, err := cacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, key+"-"+recipeVersion+".tar"), nil
}

// Cached returns the desktop layer for an image if it was built already.
func Cached(tarPath string) (string, bool) {
	p, err := LayerPath(tarPath)
	if err != nil {
		return "", false
	}
	st, err := os.Stat(p)
	return p, err == nil && st.Size() > 0
}

var building sync.Map // layer path -> *sync.Mutex

// Builder runs the install VM: ksar run with the given arguments,
// writing its output to log. It is a parameter so the UI and the CLI
// can each start ksar their own way.
type Builder func(args []string, log io.Writer) error

// Ensure returns the desktop layer for an image, building it first if
// needed. Concurrent callers for the same image share one build, in this
// process and across processes (the web UI preparing a desktop while a
// display run for the same image starts): the second waits for the first.
func Ensure(tarPath string, build Builder, log io.Writer) (string, error) {
	out, err := LayerPath(tarPath)
	if err != nil {
		return "", err
	}
	mu, _ := building.LoadOrStore(out, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer mu.(*sync.Mutex).Unlock()
	if p, ok := Cached(tarPath); ok {
		return p, nil
	}
	lock, err := os.OpenFile(out+".lock", os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		fmt.Fprintln(log, "ksar: a desktop for this image is already being installed; waiting for it")
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			return "", err
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if p, ok := Cached(tarPath); ok {
		return p, nil
	}
	info, err := Inspect(tarPath)
	if err != nil {
		return "", err
	}
	recipe, err := RecipeFor(info)
	if err != nil {
		return "", err
	}
	name := info.Name
	if name == "" {
		name = recipe.Family
	}
	fmt.Fprintf(log, "ksar: installing a minimal XFCE desktop for %s (first time only; the network reaches only %s)\n",
		name, strings.Join(recipe.Allow, ", "))
	tmp := out + ".partial"
	os.Remove(tmp)
	args := []string{"-label", "desktop install: " + name, "-mem", "2560M", "-timeout", "30m", "-net", "allow",
		"-allow", strings.Join(recipe.Allow, ","), "-save-changes", tmp, "-image", tarPath,
		"--", "sh", "-c", recipe.Script}
	if err := build(args, log); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("installing the desktop failed: %w", err)
	}
	st, err := os.Stat(tmp)
	if err != nil || st.Size() == 0 {
		os.Remove(tmp)
		return "", errors.New("installing the desktop failed: the install VM saved nothing")
	}
	if err := os.Rename(tmp, out); err != nil {
		return "", err
	}
	fmt.Fprintf(log, "ksar: desktop ready (%d MiB layer, kept for next time)\n", st.Size()>>20)
	return out, nil
}

// Session wraps a display run's command: start X and the desktop (if the
// image has one), then run args on it. With no args, the run is the
// desktop session itself and ends when it does.
func Session(args []string) []string {
	const script = `export DISPLAY=:0
if ! command -v Xorg >/dev/null 2>&1; then echo "ksar: this image has no Xorg" >&2; exit 127; fi
# -displayfd: Xorg writes the display number once it accepts clients.
rm -f /tmp/ksar-x-ready
Xorg :0 -nolisten tcp -logfile /tmp/ksar-xorg.log -displayfd 3 3>/tmp/ksar-x-ready >/dev/null 2>&1 &
x=$!
i=0
while [ ! -s /tmp/ksar-x-ready ] && [ $i -lt 300 ]; do sleep 0.1; i=$((i+1)); done
if [ ! -s /tmp/ksar-x-ready ]; then echo "ksar: X did not start; see /tmp/ksar-xorg.log in the guest" >&2; fi
if command -v startxfce4 >/dev/null 2>&1; then
  # Hide autostart items that need system services a sandbox doesn't run
  # (polkit, power management, network, screen lockers): they only show
  # error dialogs.
  a="${HOME:-/root}/.config/autostart"
  mkdir -p "$a"
  for n in xfce-polkit polkit-gnome-authentication-agent-1 polkit-mate-authentication-agent-1 \
      xfce4-power-manager light-locker xscreensaver xfce4-screensaver nm-applet blueman \
      org.gnome.SettingsDaemon.Power update-notifier geoclue-demo-agent; do
    [ -e "$a/$n.desktop" ] || printf '[Desktop Entry]\nType=Application\nHidden=true\n' > "$a/$n.desktop"
  done
  # xfce4-session runs autostart entries once the window manager, panel
  # and desktop are up: this one tells us the desktop is ready. Only
  # needed (and only safe) when a program waits for it: a signal would
  # interrupt the plain "wait" below.
  ready=0
  if [ $# -gt 0 ]; then
    trap 'ready=1' USR1
    printf '[Desktop Entry]\nType=Application\nName=ksar\nExec=sh -c "kill -USR1 %s"\n' $$ > "$a/ksar-ready.desktop"
  else
    rm -f "$a/ksar-ready.desktop"
  fi
  # A session bus for XFCE's settings daemon; dbus-run-session comes with
  # D-Bus everywhere (dbus-launch, which startxfce4 would use, does not).
  run=""
  if [ -z "$DBUS_SESSION_BUS_ADDRESS" ] && command -v dbus-run-session >/dev/null 2>&1; then run=dbus-run-session; fi
  $run startxfce4 >/tmp/ksar-desktop.log 2>&1 &
  d=$!
else
  d=$x
fi
if [ $# -gt 0 ]; then
  # Start the program once the desktop is ready, so its window is framed
  # and gets keyboard focus.
  if [ "$d" != "$x" ]; then
    i=0
    while [ "$ready" = 0 ] && [ $i -lt 600 ]; do sleep 0.1; i=$((i+1)); done
    rm -f "$a/ksar-ready.desktop"
    trap - USR1
  fi
  sleep 0.5
  exec "$@"
fi
wait $d`
	return append([]string{"sh", "-c", script, "ksar-desktop"}, args...)
}

// Layers returns the cached desktop layer tars and their total size.
func Layers() ([]string, int64) {
	dir, err := cacheDir()
	if err != nil {
		return nil, 0
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*-"+recipeVersion+".tar"))
	var total int64
	for _, p := range paths {
		if st, err := os.Stat(p); err == nil {
			total += st.Size()
		}
	}
	return paths, total
}

// Clear deletes every desktop layer and inspection result, and the disks
// built from the layers. It returns the bytes freed.
func Clear() (int64, error) {
	dir, err := cacheDir()
	if err != nil {
		return 0, err
	}
	entries, _ := os.ReadDir(dir)
	var freed int64
	for _, e := range entries {
		p := filepath.Join(dir, e.Name())
		if strings.HasSuffix(p, ".tar") {
			if c, st, ok := image.Cached(p); ok {
				freed += st.Size()
				os.Remove(c)
			}
		}
		if st, err := e.Info(); err == nil {
			freed += st.Size()
		}
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return freed, err
		}
	}
	return freed, nil
}

// Remove deletes the desktop layer for an image (call it before the
// image itself changes or goes away: the layer is keyed on it).
func Remove(tarPath string) {
	p, err := LayerPath(tarPath)
	if err != nil {
		return
	}
	if c, _, ok := image.Cached(p); ok {
		os.Remove(c)
	}
	os.Remove(p)
	if key, err := image.Key(tarPath); err == nil {
		if dir, err := cacheDir(); err == nil {
			os.Remove(filepath.Join(dir, key+".info.json"))
		}
	}
}
