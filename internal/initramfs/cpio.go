// Package initramfs writes uncompressed cpio archives in the "newc" format
// the Linux kernel unpacks into its initial root filesystem.
package initramfs

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
)

// File mode type bits (from <sys/stat.h>).
const (
	ModeDir     = 0o040000
	ModeRegular = 0o100000
	ModeCharDev = 0o020000
	ModeSymlink = 0o120000
	ModeBlkDev  = 0o060000
	ModeFIFO    = 0o010000
)

// Entry is one archive member.
type Entry struct {
	Name         string
	Mode         uint32 // type bits | permission bits
	UID, GID     uint32
	Data         []byte // file contents or symlink target
	Major, Minor uint32 // device number for device nodes
}

// Writer streams a newc cpio archive. Parent directories must be added
// before the entries inside them.
type Writer struct {
	w      io.Writer
	ino    uint32
	off    int64
	closed bool
	seen   map[string]bool
}

// NewWriter returns a Writer that writes to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w, ino: 1, seen: map[string]bool{}} }

// Has reports whether an entry with this name was already added.
func (w *Writer) Has(name string) bool { return w.seen[normalize(name)] }

// Add writes one entry. Names must be relative, clean paths, and each name
// may only be added once.
func (w *Writer) Add(e Entry) error {
	if w.closed {
		return errors.New("initramfs: write after close")
	}
	name := normalize(e.Name)
	if name == "" || name != path.Clean(name) || strings.HasPrefix(name, "../") || name == ".." {
		return fmt.Errorf("initramfs: invalid entry name %q", e.Name)
	}
	if w.seen[name] {
		return fmt.Errorf("initramfs: duplicate entry %q", name)
	}
	w.seen[name] = true
	nlink := uint32(1)
	if e.Mode&0o170000 == ModeDir {
		nlink = 2
	}
	return w.entry(name, e.Mode, e.UID, e.GID, e.Data, e.Major, e.Minor, nlink)
}

// normalize strips leading "/" or "./" and trailing "/".
func normalize(name string) string {
	name = strings.TrimPrefix(name, "./")
	name = strings.TrimLeft(name, "/")
	return strings.TrimSuffix(name, "/")
}

// AddDir adds a directory.
func (w *Writer) AddDir(name string, perm uint32) error {
	return w.Add(Entry{Name: name, Mode: ModeDir | perm&0o7777})
}

// AddFile adds a regular file.
func (w *Writer) AddFile(name string, perm uint32, data []byte) error {
	return w.Add(Entry{Name: name, Mode: ModeRegular | perm&0o7777, Data: data})
}

// AddCharDev adds a character device node.
func (w *Writer) AddCharDev(name string, perm, major, minor uint32) error {
	return w.Add(Entry{Name: name, Mode: ModeCharDev | perm&0o7777, Major: major, Minor: minor})
}

// AddSymlink adds a symbolic link pointing at target.
func (w *Writer) AddSymlink(name, target string) error {
	return w.Add(Entry{Name: name, Mode: ModeSymlink | 0o777, Data: []byte(target)})
}

// Close writes the trailer entry. It does not close the underlying writer.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.entry("TRAILER!!!", 0, 0, 0, nil, 0, 0, 1)
}

func (w *Writer) entry(name string, mode, uid, gid uint32, data []byte, major, minor, nlink uint32) error {
	ino := w.ino
	w.ino++
	if name == "TRAILER!!!" {
		ino = 0
	}
	hdr := fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		ino, mode, uid, gid, nlink, 0, len(data),
		0, 0, major, minor, len(name)+1, 0)
	if err := w.write([]byte(hdr + name + "\x00")); err != nil {
		return err
	}
	if err := w.pad(); err != nil {
		return err
	}
	if err := w.write(data); err != nil {
		return err
	}
	return w.pad()
}

func (w *Writer) write(p []byte) error {
	n, err := w.w.Write(p)
	w.off += int64(n)
	return err
}

// pad aligns the stream to 4 bytes, as newc requires after names and data.
func (w *Writer) pad() error {
	if r := w.off % 4; r != 0 {
		return w.write(make([]byte, 4-r))
	}
	return nil
}

// AddBase adds what any Linux userspace needs before PID 1 can mount
// anything: the standard top-level directories and /dev/console (so PID 1
// has stdin/stdout/stderr) and /dev/null.
func (w *Writer) AddBase() error {
	for _, d := range []string{"dev", "proc", "sys", "tmp", "run"} {
		if err := w.AddDir(d, 0o755); err != nil {
			return err
		}
	}
	if err := w.AddCharDev("dev/console", 0o600, 5, 1); err != nil {
		return err
	}
	return w.AddCharDev("dev/null", 0o666, 1, 3)
}

// Minimal builds the smallest useful initramfs: the base entries and init
// as /init.
func Minimal(w io.Writer, init []byte) error {
	cw := NewWriter(w)
	if err := cw.AddBase(); err != nil {
		return err
	}
	if err := cw.AddFile("init", 0o755, init); err != nil {
		return err
	}
	return cw.Close()
}

// AddTar copies a tar archive (e.g. `docker export` output) into the
// initramfs. Entries whose names were already added are skipped, so
// entries added first take precedence. Missing parent directories are
// created. Hard links become independent copies.
func (w *Writer) AddTar(r io.Reader) error {
	tr := tar.NewReader(r)
	files := map[string][]byte{} // regular files, for resolving hard links
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("initramfs: read tar: %w", err)
		}
		name := normalize(h.Name)
		if name == "" || name == "." || w.Has(name) {
			continue
		}
		if err := w.addParents(name); err != nil {
			return err
		}
		e := Entry{Name: name, UID: uint32(h.Uid), GID: uint32(h.Gid)}
		perm := uint32(h.Mode) & 0o7777
		switch h.Typeflag {
		case tar.TypeDir:
			e.Mode = ModeDir | perm
		case tar.TypeReg:
			e.Mode = ModeRegular | perm
			if e.Data, err = io.ReadAll(tr); err != nil {
				return fmt.Errorf("initramfs: read %s: %w", name, err)
			}
			files[name] = e.Data
		case tar.TypeLink:
			target := normalize(h.Linkname)
			data, ok := files[target]
			if !ok {
				return fmt.Errorf("initramfs: hard link %s points to unknown file %s", name, target)
			}
			e.Mode, e.Data = ModeRegular|perm, data
			files[name] = data
		case tar.TypeSymlink:
			e.Mode, e.Data = ModeSymlink|0o777, []byte(h.Linkname)
		case tar.TypeChar:
			e.Mode, e.Major, e.Minor = ModeCharDev|perm, uint32(h.Devmajor), uint32(h.Devminor)
		case tar.TypeBlock:
			e.Mode, e.Major, e.Minor = ModeBlkDev|perm, uint32(h.Devmajor), uint32(h.Devminor)
		case tar.TypeFifo:
			e.Mode = ModeFIFO | perm
		default:
			continue // e.g. sockets or vendor extensions: nothing to unpack
		}
		if err := w.Add(e); err != nil {
			return err
		}
	}
}

func (w *Writer) addParents(name string) error {
	dir := path.Dir(name)
	if dir == "." || w.Has(dir) {
		return nil
	}
	if err := w.addParents(dir); err != nil {
		return err
	}
	return w.AddDir(dir, 0o755)
}
