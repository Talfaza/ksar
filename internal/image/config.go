package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// Config is the part of a container image's configuration that a plain
// filesystem export (docker export) loses: the environment (for example
// the PATH entry where a Go or Java image installs its toolchain) and the
// default working directory. It lives next to the tar as NAME.tar.json.
type Config struct {
	Env        []string `json:"env,omitempty"`
	WorkingDir string   `json:"workdir,omitempty"`
}

// ConfigPath is where the configuration for tarPath is stored.
func ConfigPath(tarPath string) string { return tarPath + ".json" }

const maxConfigSize = 1 << 20

// LoadConfig reads the configuration for tarPath. A missing file is not an
// error: the image just has no extra configuration.
func LoadConfig(tarPath string) (Config, error) {
	var c Config
	f, err := os.Open(ConfigPath(tarPath))
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	if err != nil {
		return c, err
	}
	if len(b) > maxConfigSize {
		return c, fmt.Errorf("%s is larger than 1 MB", ConfigPath(tarPath))
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", ConfigPath(tarPath), err)
	}
	for _, e := range c.Env {
		if k, _, ok := strings.Cut(e, "="); !ok || k == "" || strings.ContainsRune(e, 0) {
			return c, fmt.Errorf("%s: bad environment entry %q", ConfigPath(tarPath), e)
		}
	}
	if c.WorkingDir != "" && !strings.HasPrefix(c.WorkingDir, "/") {
		return c, fmt.Errorf("%s: working directory %q is not absolute", ConfigPath(tarPath), c.WorkingDir)
	}
	return c, nil
}

// SaveConfig writes the configuration for tarPath.
func SaveConfig(tarPath string, c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(ConfigPath(tarPath), append(b, '\n'), 0o644)
}
