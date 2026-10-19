package initramfs

import (
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
	for _, name := range []string{"", "../etc/passwd", "a/../../b", "a//b"} {
		if err := w.AddFile(name, 0o644, nil); err == nil {
			t.Errorf("AddFile(%q) succeeded", name)
		}
	}
}
