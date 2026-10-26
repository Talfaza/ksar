package desktop

import (
	"archive/tar"
	"bytes"
	"strings"
	"testing"
)

func tarOf(t *testing.T, entries map[string]string, links map[string]string) *bytes.Reader {
	t.Helper()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for name, body := range entries {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	for name, target := range links {
		tw.WriteHeader(&tar.Header{Name: name, Linkname: target, Typeflag: tar.TypeSymlink})
	}
	tw.Close()
	return bytes.NewReader(b.Bytes())
}

func TestInspect(t *testing.T) {
	// Debian-style: etc/os-release is a symlink into usr/lib.
	info, err := inspect(tarOf(t, map[string]string{
		"./usr/lib/os-release": "PRETTY_NAME=\"Debian GNU/Linux 12 (bookworm)\"\nID=debian\nVERSION_ID=\"12\"\n",
		"usr/bin/ls":           "x",
	}, map[string]string{"./etc/os-release": "../usr/lib/os-release"}))
	if err != nil || info.Distro != "debian" || info.Version != "12" || info.HasX || info.HasXfce {
		t.Errorf("debian: %+v %v", info, err)
	}
	info, _ = inspect(tarOf(t, map[string]string{
		"etc/os-release": "ID=alpine\n",
		"usr/lib/xorg/modules/drivers/modesetting_drv.so": "x",
		"usr/bin/startxfce4":                              "x",
	}, nil))
	if info.Distro != "alpine" || !info.Ready() {
		t.Errorf("alpine desktop: %+v", info)
	}
}

func TestRecipeFor(t *testing.T) {
	for _, c := range []struct {
		info   Info
		family string
	}{
		{Info{Distro: "debian"}, "debian"},
		{Info{Distro: "ubuntu", Like: "debian"}, "ubuntu"},
		{Info{Distro: "linuxmint", Like: "ubuntu debian"}, "ubuntu"},
		{Info{Distro: "alpine"}, "alpine"},
		{Info{Distro: "arch"}, "arch"},
		{Info{Distro: "fedora"}, "fedora"},
		{Info{Distro: "rocky", Like: "rhel centos fedora"}, ""},
		{Info{Distro: "opensuse-tumbleweed", Like: "opensuse suse"}, ""},
		{Info{}, ""},
	} {
		r, err := RecipeFor(c.info)
		if c.family == "" {
			if err == nil {
				t.Errorf("%+v: got recipe %s, want an error", c.info, r.Family)
			}
			continue
		}
		if err != nil || r.Family != c.family || len(r.Allow) == 0 || r.Script == "" {
			t.Errorf("%+v: %+v %v", c.info, r.Family, err)
		}
	}
}

func TestSession(t *testing.T) {
	s := Session(nil)
	if s[0] != "sh" || s[1] != "-c" || len(s) != 4 || !strings.Contains(s[2], "Xorg :0") {
		t.Errorf("desktop-only session: %q", s)
	}
	s = Session([]string{"xterm", "-e", "top"})
	if strings.Join(s[4:], " ") != "xterm -e top" {
		t.Errorf("command not passed through: %q", s[4:])
	}
}
