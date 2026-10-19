package initramfs

import (
	"archive/tar"
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

func TestFormat(t *testing.T) {
	var buf bytes.Buffer
	if err := Minimal(&buf, []byte("#!/bin/sh\n")); err != nil {
		t.Fatal(err)
	}
	b := buf.Bytes()
	if len(b)%4 != 0 {
		t.Errorf("archive length %d not 4-byte aligned", len(b))
	}
	if !bytes.HasPrefix(b, []byte("070701")) {
		t.Error("missing newc magic")
	}
	if !bytes.Contains(b, []byte("TRAILER!!!\x00")) {
		t.Error("missing trailer")
	}
}

// Cross-check against the system cpio tool when it is installed.
func TestReadableByCpio(t *testing.T) {
	if _, err := exec.LookPath("cpio"); err != nil {
		t.Skip("cpio not installed")
	}
	var buf bytes.Buffer
	if err := Minimal(&buf, []byte("hello")); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("cpio", "-t", "-v")
	cmd.Stdin = &buf
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cpio: %v\n%s", err, out)
	}
	for _, want := range []string{"dev/console", "dev/null", "init", "proc"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("cpio listing missing %q:\n%s", want, out)
		}
	}
}

func TestRejectsBadNames(t *testing.T) {
	w := NewWriter(&bytes.Buffer{})
	for _, name := range []string{"", "../etc/passwd", "a/../../b", "a//b", ".."} {
		if err := w.AddFile(name, 0o644, nil); err == nil {
			t.Errorf("AddFile(%q) succeeded", name)
		}
	}
	w.AddFile("x", 0o644, nil)
	if err := w.AddFile("/x", 0o644, nil); err == nil {
		t.Error("duplicate entry accepted")
	}
}

func TestAddTar(t *testing.T) {
	var tb bytes.Buffer
	tw := tar.NewWriter(&tb)
	add := func(h *tar.Header, data string) {
		h.Size = int64(len(data))
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		tw.Write([]byte(data))
	}
	add(&tar.Header{Name: "./dev/", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	add(&tar.Header{Name: "./dev/console", Typeflag: tar.TypeChar, Mode: 0o600, Devmajor: 9}, "")
	add(&tar.Header{Name: "bin/busybox", Typeflag: tar.TypeReg, Mode: 0o755}, "BB") // parent dir missing
	add(&tar.Header{Name: "bin/sh", Typeflag: tar.TypeSymlink, Linkname: "/bin/busybox"}, "")
	add(&tar.Header{Name: "bin/ls", Typeflag: tar.TypeLink, Linkname: "bin/busybox", Mode: 0o755}, "")
	add(&tar.Header{Name: "home/u/f", Typeflag: tar.TypeReg, Mode: 0o4750, Uid: 1000, Gid: 100}, "x")
	tw.Close()

	var out bytes.Buffer
	w := NewWriter(&out)
	if err := w.AddBase(); err != nil {
		t.Fatal(err)
	}
	if err := w.AddTar(&tb); err != nil {
		t.Fatal(err)
	}
	w.Close()
	for _, n := range []string{"bin", "bin/busybox", "bin/sh", "bin/ls", "home", "home/u", "home/u/f"} {
		if !w.Has(n) {
			t.Errorf("missing %s", n)
		}
	}
	// Our dev/console (major 5) must win over the image's (major 9).
	if bytes.Contains(out.Bytes(), []byte("dev/console")) &&
		bytes.Count(out.Bytes(), []byte("dev/console\x00")) > 1 {
		t.Error("dev/console added twice")
	}
	if _, err := exec.LookPath("cpio"); err == nil {
		cmd := exec.Command("cpio", "-t", "-v", "--numeric-uid-gid")
		cmd.Stdin = &out
		listing, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("cpio: %v\n%s", err, listing)
		}
		for _, want := range []string{"-rwsr-x---   1 1000     100", "bin/sh -> /bin/busybox", "crw-------   1 0        0          5,   1"} {
			if !strings.Contains(string(listing), want) {
				t.Errorf("listing missing %q:\n%s", want, listing)
			}
		}
	}
}
