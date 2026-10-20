package sandbox

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// PackStats describes a packed directory.
type PackStats struct {
	Files, Dirs, Symlinks int
	Skipped               int // devices, sockets, pipes
	Bytes                 int64
}

// PackDir writes the contents of the host directory root to w as a tar
// archive, for copying into a guest.
//
// The directory is untrusted (it may be the very project being sandboxed),
// so symlinks are stored as links and never followed: a link such as
// "notes -> ~/.ssh/id_rsa" must not pull the user's key into the guest.
// Only the root itself may be a symlink. Device files, sockets and pipes
// are skipped. Ownership is not recorded; the guest owns the copy.
func PackDir(w io.Writer, root string) (PackStats, error) {
	var st PackStats
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return st, err
	}
	if info, err := os.Stat(real); err != nil {
		return st, err
	} else if !info.IsDir() {
		return st, fmt.Errorf("%s is not a directory", root)
	}
	tw := tar.NewWriter(w)
	err = filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(real, p)
		if err != nil || rel == "." {
			return err
		}
		info, err := d.Info() // Lstat: a symlink describes itself
		if err != nil {
			return err
		}
		hdr := &tar.Header{
			Name:    filepath.ToSlash(rel),
			Mode:    int64(info.Mode().Perm()), // setuid/setgid bits are dropped
			ModTime: info.ModTime(),
			Format:  tar.FormatPAX,
		}
		switch {
		case info.Mode().IsDir():
			hdr.Typeflag, hdr.Name = tar.TypeDir, hdr.Name+"/"
			st.Dirs++
			return tw.WriteHeader(hdr)
		case info.Mode()&fs.ModeSymlink != 0:
			target, err := os.Readlink(p)
			if err != nil {
				return err
			}
			hdr.Typeflag, hdr.Linkname = tar.TypeSymlink, target
			st.Symlinks++
			return tw.WriteHeader(hdr)
		case info.Mode().IsRegular():
			f, err := os.Open(p)
			if err != nil {
				return err
			}
			defer f.Close()
			hdr.Typeflag, hdr.Size = tar.TypeReg, info.Size()
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			n, err := io.Copy(tw, io.LimitReader(f, info.Size()))
			if err != nil {
				return err
			}
			if n != info.Size() {
				return fmt.Errorf("%s changed size while being copied", rel)
			}
			st.Files++
			st.Bytes += n
			return nil
		default:
			st.Skipped++
			return nil
		}
	})
	if err != nil {
		return st, err
	}
	return st, tw.Close()
}
