package ui

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
)

// Archive uploads (.zip, .tar, .tar.gz, .tgz) are unpacked into a folder
// upload, so a project can be sent as one file. The archive is as
// untrusted as anything else from the browser: every name must stay inside
// the folder, only regular files and directories are created (symlinks,
// devices and the like are skipped), and the same size and file-count
// limits as folder uploads apply to what it expands to.

// IsArchive reports whether a file name looks like a supported archive.
func IsArchive(name string) bool {
	n := strings.ToLower(name)
	for _, ext := range []string{".zip", ".tar", ".tar.gz", ".tgz"} {
		if strings.HasSuffix(n, ext) {
			return true
		}
	}
	return false
}

func archiveBase(name string) string {
	n := filepath.Base(name)
	for _, ext := range []string{".tar.gz", ".tgz", ".zip", ".tar"} {
		if strings.HasSuffix(strings.ToLower(n), ext) {
			return n[:len(n)-len(ext)]
		}
	}
	return n
}

// addArchive unpacks an uploaded archive into a new folder upload.
func (s *uploadStore) addArchive(name string, r io.Reader) (upload, error) {
	// zip needs random access: keep the archive in a temp file first.
	tmp, err := os.CreateTemp(s.dir, "archive-*")
	if err != nil {
		return upload{}, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	n, err := io.Copy(tmp, io.LimitReader(r, maxDirUploadBytes+1))
	if err != nil {
		return upload{}, err
	}
	if n > maxDirUploadBytes {
		return upload{}, fmt.Errorf("archive is larger than %d MB", maxDirUploadBytes>>20)
	}

	u := upload{ID: newID(), Name: archiveBase(name), Dir: true}
	if u.Name == "" || u.Name == "." {
		u.Name = "project"
	}
	u.path = filepath.Join(s.dir, u.ID)
	if err := os.Mkdir(u.path, 0o700); err != nil {
		return u, err
	}
	fail := func(err error) (upload, error) {
		os.RemoveAll(u.path)
		return u, err
	}
	x := &extractor{root: u.path}
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".zip"):
		err = x.zip(tmp, n)
	case strings.HasSuffix(lower, ".tar"):
		tmp.Seek(0, io.SeekStart)
		err = x.tar(tmp)
	default:
		tmp.Seek(0, io.SeekStart)
		var gz *gzip.Reader
		if gz, err = gzip.NewReader(bufio.NewReader(tmp)); err == nil {
			err = x.tar(gz)
		}
	}
	if err != nil {
		return fail(fmt.Errorf("%s: %w", filepath.Base(name), err))
	}
	if x.files == 0 {
		return fail(errors.New("the archive has no files"))
	}
	// GitHub-style archives wrap everything in one folder: use its
	// contents (and its name) instead.
	if entries, err := os.ReadDir(u.path); err == nil && len(entries) == 1 && entries[0].IsDir() {
		inner := filepath.Join(u.path, entries[0].Name())
		flat := u.path + ".flat"
		if os.Rename(inner, flat) == nil && os.Remove(u.path) == nil && os.Rename(flat, u.path) == nil {
			u.Name = entries[0].Name()
		}
	}
	u.Size, u.Files = x.size, x.files
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.total+u.Size > maxUploadTotal {
		os.RemoveAll(u.path)
		return u, fmt.Errorf("uploads exceed %d MB in total; finish or remove some runs first", maxUploadTotal>>20)
	}
	s.total += u.Size
	s.files[u.ID] = u
	return u, nil
}

type extractor struct {
	root         string
	size         int64
	files        int
	skippedLinks int
}

// file creates one regular file from an archive entry.
func (x *extractor) file(name string, mode os.FileMode, r io.Reader) error {
	rel, err := relPath(strings.TrimPrefix(name, "./"))
	if err != nil {
		return err
	}
	if x.files >= maxDirFiles {
		return fmt.Errorf("more than %d files", maxDirFiles)
	}
	target := filepath.Join(x.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	perm := os.FileMode(0o644)
	if mode&0o111 != 0 {
		perm = 0o755
	}
	f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, perm)
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	// Count what is actually written, not what the header claims: that is
	// what stops a decompression bomb.
	left := maxDirUploadBytes - x.size
	n, err := io.Copy(f, io.LimitReader(r, left+1))
	f.Close()
	if err != nil {
		return fmt.Errorf("%s: %w", rel, err)
	}
	x.size += n
	if n > left {
		return fmt.Errorf("unpacks to more than %d MB", maxDirUploadBytes>>20)
	}
	x.files++
	return nil
}

func (x *extractor) dir(name string) error {
	rel, err := relPath(strings.TrimPrefix(strings.TrimSuffix(name, "/"), "./"))
	if err != nil {
		if name == "./" || name == "." {
			return nil
		}
		return err
	}
	return os.MkdirAll(filepath.Join(x.root, filepath.FromSlash(rel)), 0o755)
}

func (x *extractor) zip(r io.ReaderAt, size int64) error {
	zr, err := zip.NewReader(r, size)
	if err != nil {
		return err
	}
	for _, f := range zr.File {
		switch mode := f.Mode(); {
		case mode.IsDir():
			if err := x.dir(f.Name); err != nil {
				return err
			}
		case mode.IsRegular():
			rc, err := f.Open()
			if err != nil {
				return fmt.Errorf("%s: %w", f.Name, err)
			}
			err = x.file(f.Name, mode, rc)
			rc.Close()
			if err != nil {
				return err
			}
		default:
			x.skippedLinks++ // symlinks and anything unusual are not created
		}
	}
	return nil
}

func (x *extractor) tar(r io.Reader) error {
	tr := tar.NewReader(r)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if path.Base(h.Name) == "pax_global_header" {
			continue
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := x.dir(h.Name); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := x.file(h.Name, os.FileMode(h.Mode), tr); err != nil {
				return err
			}
		default:
			x.skippedLinks++
		}
	}
}
