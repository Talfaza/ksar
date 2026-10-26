package ui

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func zipOf(t *testing.T, entries map[string]string, symlink string) []byte {
	var b bytes.Buffer
	zw := zip.NewWriter(&b)
	for name, body := range entries {
		w, _ := zw.Create(name)
		w.Write([]byte(body))
	}
	if symlink != "" {
		h := &zip.FileHeader{Name: symlink}
		h.SetMode(os.ModeSymlink | 0o777)
		w, _ := zw.CreateHeader(h)
		w.Write([]byte("/etc/passwd"))
	}
	zw.Close()
	return b.Bytes()
}

func TestArchiveUpload(t *testing.T) {
	s, err := newUploadStore()
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()

	// A GitHub-style zip: one wrapping folder, which is dropped.
	u, err := s.addArchive("proj.zip", bytes.NewReader(zipOf(t, map[string]string{
		"proj-main/main.py": "print(1)", "proj-main/pkg/a.txt": "a"}, "proj-main/evil-link")))
	if err != nil {
		t.Fatal(err)
	}
	if u.Name != "proj-main" || u.Files != 2 || !u.Dir {
		t.Errorf("upload = %+v", u)
	}
	if b, err := os.ReadFile(filepath.Join(u.path, "main.py")); err != nil || string(b) != "print(1)" {
		t.Errorf("main.py not at the top: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(u.path, "evil-link")); !os.IsNotExist(err) {
		t.Error("a symlink from the archive was created")
	}

	// Escapes are refused, whatever the format.
	for name, data := range map[string][]byte{
		"slip.zip": zipOf(t, map[string]string{"../../escape.txt": "x"}, ""),
		"abs.zip":  zipOf(t, map[string]string{"/etc/cron.d/x": "x"}, ""),
	} {
		if _, err := s.addArchive(name, bytes.NewReader(data)); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	var tb bytes.Buffer
	gz := gzip.NewWriter(&tb)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "../up.txt", Mode: 0o644, Size: 1, Typeflag: tar.TypeReg})
	tw.Write([]byte("x"))
	tw.Close()
	gz.Close()
	if _, err := s.addArchive("slip.tar.gz", &tb); err == nil {
		t.Error("tar with ../ accepted")
	}

	// A small archive that expands past the folder limit is stopped while
	// writing, not trusted by its headers.
	var big bytes.Buffer
	zw := zip.NewWriter(&big)
	w, _ := zw.Create("zeros")
	chunk := make([]byte, 1<<20)
	for written := int64(0); written <= maxDirUploadBytes; written += int64(len(chunk)) {
		w.Write(chunk)
	}
	zw.Close()
	if big.Len() > 4<<20 {
		t.Fatalf("test archive is %d bytes; expected it to compress well", big.Len())
	}
	if _, err := s.addArchive("bomb.zip", bytes.NewReader(big.Bytes())); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Errorf("decompression bomb: %v", err)
	}
	entries, _ := os.ReadDir(s.dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "archive-") {
			t.Errorf("temporary archive left behind: %s", e.Name())
		}
	}
}
