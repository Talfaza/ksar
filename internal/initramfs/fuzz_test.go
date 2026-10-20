package initramfs

import (
	"archive/tar"
	"bytes"
	"io"
	"testing"
)

// FuzzAddTar converts arbitrary (often corrupt) tar streams. Images come
// from users, not guests, but a hostile image must still only fail.
func FuzzAddTar(f *testing.F) {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	tw.WriteHeader(&tar.Header{Name: "a/b", Typeflag: tar.TypeReg, Size: 1, Mode: 0o644})
	tw.Write([]byte("x"))
	tw.WriteHeader(&tar.Header{Name: "a/c", Typeflag: tar.TypeLink, Linkname: "a/b"})
	tw.WriteHeader(&tar.Header{Name: "../evil", Typeflag: tar.TypeReg})
	tw.Close()
	f.Add(b.Bytes())
	f.Fuzz(func(t *testing.T, data []byte) {
		w := NewWriter(io.Discard)
		w.AddBase()
		w.AddTar(bytes.NewReader(data))
		w.Close()
	})
}
