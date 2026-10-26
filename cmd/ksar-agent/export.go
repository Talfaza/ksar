package main

import (
	"archive/tar"
	"errors"
	"io"
	"io/fs"
	"path"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"

	"ksar/internal/agent/proto"
)

// exportAlways are never part of an export: scratch space, and files the
// agent itself wrote for this run.
var exportAlways = []string{"tmp", "run", "var/tmp", "etc/resolv.conf", "etc/hosts", "etc/X11/xorg.conf.d/10-ksar.conf"}

// export streams the overlay's writable layer, i.e. everything the guest
// added, changed or deleted, as a tar. Deleted files are overlayfs
// whiteouts (character devices 0:0) and stay that way in the tar, so the
// result works as an overlay layer on top of the same image.
func (a *agent) export(m proto.Export) error {
	if upper == nil {
		return a.out.WriteJSON(proto.TypeResult, proto.Result{Error: "no writable layer (the run has no image)"})
	}
	exclude := append(append([]string(nil), exportAlways...), m.Exclude...)
	skip := func(p string) bool {
		for _, e := range exclude {
			e = strings.Trim(e, "/")
			if p == e || strings.HasPrefix(p, e+"/") {
				return true
			}
		}
		return false
	}

	pr, pw := io.Pipe()
	result := make(chan proto.Result, 1)
	go func() {
		var res proto.Result
		tw := tar.NewWriter(pw)
		err := fs.WalkDir(upper.FS(), ".", func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if p == "." {
				return nil
			}
			if skip(p) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.Type()&fs.ModeSocket != 0 {
				return nil // a live socket (e.g. gpg-agent's) means nothing in a stored layer
			}
			if err := writeEntry(tw, p); err != nil {
				return err
			}
			res.Files++
			return nil
		})
		if err == nil {
			err = tw.Close()
		}
		if err != nil {
			res.Error = err.Error()
		}
		pw.CloseWithError(err)
		result <- res
	}()
	buf := make([]byte, 60<<10)
	for {
		n, err := pr.Read(buf)
		if n > 0 {
			if werr := a.out.Write(proto.TypeExportData, buf[:n]); werr != nil {
				pr.CloseWithError(werr)
				<-result
				return werr
			}
		}
		if err != nil {
			break
		}
	}
	return a.out.WriteJSON(proto.TypeResult, <-result)
}

func writeEntry(tw *tar.Writer, p string) error {
	fi, err := upper.Lstat(p)
	if err != nil {
		return err
	}
	link := ""
	if fi.Mode()&fs.ModeSymlink != 0 {
		if link, err = upper.Readlink(p); err != nil {
			return err
		}
	}
	hdr, err := tar.FileInfoHeader(fi, link)
	if err != nil {
		return err
	}
	hdr.Name = p
	hdr.Format = tar.FormatPAX
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		hdr.Uid, hdr.Gid = int(st.Uid), int(st.Gid)
		hdr.Uname, hdr.Gname = "", ""
		if hdr.Typeflag == tar.TypeChar || hdr.Typeflag == tar.TypeBlock {
			hdr.Devmajor, hdr.Devminor = int64(unix.Major(st.Rdev)), int64(unix.Minor(st.Rdev))
		}
	}
	if fi.IsDir() {
		hdr.Name += "/"
	}
	var f interface {
		io.Reader
		Fd() uintptr
		Close() error
	}
	if fi.Mode().IsRegular() || fi.IsDir() {
		file, err := upper.Open(p)
		if err != nil {
			return err
		}
		defer file.Close()
		f = file
		// Keep the xattrs that matter: an opaque directory (it replaced a
		// deleted one) and file capabilities (e.g. ping).
		for _, name := range []string{"trusted.overlay.opaque", "security.capability"} {
			if v, ok := getxattr(int(file.Fd()), name); ok {
				if hdr.PAXRecords == nil {
					hdr.PAXRecords = map[string]string{}
				}
				hdr.PAXRecords["SCHILY.xattr."+name] = v
			}
		}
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if f != nil && fi.Mode().IsRegular() {
		// Exactly the size in the header, even if a background process is
		// still changing the file: pad if it shrank, cut if it grew.
		n, err := io.CopyN(tw, f, hdr.Size)
		if err != nil && err != io.EOF {
			return errors.New(path.Clean(p) + ": " + err.Error())
		}
		if n < hdr.Size {
			if _, err := io.CopyN(tw, zeros{}, hdr.Size-n); err != nil {
				return err
			}
		}
	}
	return nil
}

func getxattr(fd int, name string) (string, bool) {
	buf := make([]byte, 256)
	n, err := unix.Fgetxattr(fd, name, buf)
	if err != nil || n <= 0 {
		return "", false
	}
	return string(buf[:n]), true
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) { clear(p); return len(p), nil }
