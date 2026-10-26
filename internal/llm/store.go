package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

// The model files, pinned to one revision and checked by hash: what runs
// is exactly what was tested.
const (
	ModelName     = "SmolLM2-135M-Instruct"
	modelRevision = "12fd25f77366fa6b3b4b768ec3050bf629380bac"
	modelBaseURL  = "https://huggingface.co/HuggingFaceTB/SmolLM2-135M-Instruct/resolve/" + modelRevision + "/"
	maxContext    = 1024
)

var modelFiles = []struct {
	name   string
	sha256 string
	size   int64
}{
	{"tokenizer.json", "9ca9acddb6525a194ec8ac7a87f24fbba7232a9a15ffa1af0c1224fcd888e47c", 2104556},
	{"model.safetensors", "5af571cbf074e6d21a03528d2330792e532ca608f24ac70a143f6b369968ab8c", 269060552},
}

// DownloadSize is the total size of the model files.
func DownloadSize() int64 {
	var n int64
	for _, f := range modelFiles {
		n += f.size
	}
	return n
}

// Dir is where the model lives ($XDG_CACHE_HOME/ksar/models/...).
func Dir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "ksar", "models", "smollm2-135m-instruct-"+modelRevision[:8]), nil
}

// Installed reports whether the model files are present (their hashes
// were checked when they were downloaded).
func Installed() bool {
	dir, err := Dir()
	if err != nil {
		return false
	}
	for _, f := range modelFiles {
		st, err := os.Stat(filepath.Join(dir, f.name))
		if err != nil || st.Size() != f.size {
			return false
		}
	}
	return true
}

// Download fetches the model files, reporting progress (bytes done of
// total). Each file is verified before it is put in place.
func Download(ctx context.Context, progress func(done, total int64)) error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	total, done := DownloadSize(), int64(0)
	client := &http.Client{Timeout: 30 * time.Minute}
	for _, f := range modelFiles {
		dst := filepath.Join(dir, f.name)
		if st, err := os.Stat(dst); err == nil && st.Size() == f.size {
			done += f.size
			continue
		}
		if err := fetch(ctx, client, modelBaseURL+f.name, dst, f.sha256, f.size, func(n int64) {
			if progress != nil {
				progress(done+n, total)
			}
		}); err != nil {
			return fmt.Errorf("%s: %w", f.name, err)
		}
		done += f.size
	}
	return nil
}

func fetch(ctx context.Context, client *http.Client, url, dst, want string, size int64, progress func(int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download: %s", resp.Status)
	}
	tmp := dst + ".partial"
	out, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	h := sha256.New()
	var n int64
	buf := make([]byte, 1<<20)
	for {
		m, rerr := resp.Body.Read(buf)
		if m > 0 {
			if n+int64(m) > size {
				out.Close()
				return errors.New("download is larger than expected")
			}
			h.Write(buf[:m])
			if _, err := out.Write(buf[:m]); err != nil {
				out.Close()
				return err
			}
			n += int64(m)
			progress(n)
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			out.Close()
			return rerr
		}
	}
	if err := out.Close(); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want || n != size {
		return fmt.Errorf("checksum mismatch (got %s): the download was corrupted or altered", got)
	}
	return os.Rename(tmp, dst)
}

// Remove deletes the model files.
func Remove() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// LoadAssistant loads the installed model, ready to rank commands.
func LoadAssistant() (*Assistant, error) {
	if !Installed() {
		return nil, errors.New("the assistant model isn't downloaded")
	}
	dir, _ := Dir()
	tok, err := LoadTokenizer(filepath.Join(dir, "tokenizer.json"))
	if err != nil {
		return nil, err
	}
	m, err := Load(filepath.Join(dir, "model.safetensors"), SmolLM2_135M, maxContext)
	if err != nil {
		return nil, err
	}
	return NewAssistant(m, tok)
}
