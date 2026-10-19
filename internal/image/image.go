// Package image turns root filesystem tarballs (e.g. `docker export`
// output) into read-only EROFS disk images, cached on the host.
//
// EROFS is built for exactly this (read-only container layers), and
// mkfs.erofs reads a tar directly, keeping file ownership and modes
// without needing root on the host.
package image

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// CacheDir returns where built images are kept ($XDG_CACHE_HOME/ksar/images).
func CacheDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ksar", "images"), nil
}

// FromTar returns the path of an EROFS image built from tarPath, building
// it on first use. The cache key covers the tar's path, size and mtime, so
// a re-exported tar is rebuilt.
func FromTar(tarPath string) (string, error) {
	abs, err := filepath.Abs(tarPath)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("image %s is not a regular file", tarPath)
	}
	h := sha256.Sum256([]byte(abs + "\x00" + strconv.FormatInt(st.Size(), 10) + "\x00" +
		strconv.FormatInt(st.ModTime().UnixNano(), 10)))
	key := hex.EncodeToString(h[:12])

	dir, err := CacheDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	out := filepath.Join(dir, key+".erofs")
	if _, err := os.Stat(out); err == nil {
		return out, nil
	}

	mkfs, err := exec.LookPath("mkfs.erofs")
	if err != nil {
		return "", errors.New("mkfs.erofs not found: install erofs-utils (Fedora: dnf install erofs-utils, Debian/Ubuntu: apt install erofs-utils)")
	}
	// Build under a temporary name and rename, so concurrent runs and
	// interrupted builds never leave a half-written image in the cache.
	tmp, err := os.CreateTemp(dir, key+".*.tmp")
	if err != nil {
		return "", err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	cmd := exec.Command(mkfs, "--quiet", "--tar=f", "-T0", tmp.Name(), abs)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("mkfs.erofs %s: %v\n%s", tarPath, err, output)
	}
	if err := padToSector(tmp.Name()); err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), out); err != nil {
		return "", err
	}
	return out, nil
}

// padToSector grows a file to a multiple of 512 bytes, as block devices
// require.
func padToSector(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if r := st.Size() % 512; r != 0 {
		return os.Truncate(path, st.Size()+512-r)
	}
	return nil
}
