// Package initramfs writes uncompressed cpio archives in the "newc" format
// the Linux kernel unpacks into its initial root filesystem.
package initramfs

import (
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
)

// Writer streams a newc cpio archive. Parent directories must be added
// before the entries inside them.
type Writer struct {
	w      io.Writer
	ino    uint32
	off    int64
	closed bool
}

// NewWriter returns a Writer that writes to w.
func NewWriter(w io.Writer) *Writer { return &Writer{w: w, ino: 1} }

// AddDir adds a directory.
func (w *Writer) AddDir(name string, perm uint32) error {
	return w.add(name, ModeDir|perm&0o7777, nil, 0, 0)
}

// AddFile adds a regular file.
func (w *Writer) AddFile(name string, perm uint32, data []byte) error {
	return w.add(name, ModeRegular|perm&0o7777, data, 0, 0)
}

// AddCharDev adds a character device node.
func (w *Writer) AddCharDev(name string, perm, major, minor uint32) error {
	return w.add(name, ModeCharDev|perm&0o7777, nil, major, minor)
}

// AddSymlink adds a symbolic link pointing at target.
func (w *Writer) AddSymlink(name, target string) error {
	return w.add(name, ModeSymlink|0o777, []byte(target), 0, 0)
}

// Close writes the trailer entry. It does not close the underlying writer.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	return w.entry("TRAILER!!!", 0, nil, 0, 0, 1)
}

func (w *Writer) add(name string, mode uint32, data []byte, major, minor uint32) error {
	if w.closed {
		return fmt.Errorf("initramfs: write after close")
	}
	clean := strings.TrimPrefix(path.Clean("/"+name), "/")
	if clean == "" || clean != strings.TrimPrefix(name, "/") {
		return fmt.Errorf("initramfs: invalid entry name %q", name)
	}
	nlink := uint32(1)
	if mode&0o170000 == ModeDir {
		nlink = 2
	}
	return w.entry(clean, mode, data, major, minor, nlink)
}

func (w *Writer) entry(name string, mode uint32, data []byte, major, minor, nlink uint32) error {
	ino := w.ino
	w.ino++
	if name == "TRAILER!!!" {
		ino = 0
	}
	hdr := fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
		ino, mode, 0, 0, nlink, 0, len(data),
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

// Minimal builds the smallest useful initramfs: the directories a Linux
// userspace expects, a /dev/console node (so PID 1 has stdin/out/err
// before devtmpfs exists), and init as /init.
func Minimal(w io.Writer, init []byte) error {
	cw := NewWriter(w)
	for _, d := range []string{"dev", "proc", "sys", "tmp", "run"} {
		if err := cw.AddDir(d, 0o755); err != nil {
			return err
		}
	}
	if err := cw.AddCharDev("dev/console", 0o600, 5, 1); err != nil {
		return err
	}
	if err := cw.AddCharDev("dev/null", 0o666, 1, 3); err != nil {
		return err
	}
	if err := cw.AddFile("init", 0o755, init); err != nil {
		return err
	}
	return cw.Close()
}
