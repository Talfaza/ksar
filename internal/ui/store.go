package ui

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"ksar/internal/image"
)

// --- Uploads: files the browser sends to copy into a guest. ---

const (
	maxUploadBytes    = 64 << 20  // one file
	maxDirUploadBytes = 512 << 20 // one folder, all files together
	maxDirFiles       = 20000
	maxUploadTotal    = 1 << 30
	maxRelPath        = 4096
)

type upload struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	Dir   bool   `json:"dir"`
	Files int    `json:"files,omitempty"`
	path  string
}

type uploadStore struct {
	mu    sync.Mutex
	dir   string
	files map[string]upload
	total int64
}

func newUploadStore() (*uploadStore, error) {
	dir, err := os.MkdirTemp("", "ksar-ui-uploads-")
	if err != nil {
		return nil, err
	}
	return &uploadStore{dir: dir, files: map[string]upload{}}, nil
}

// add stores r (at most maxUploadBytes) and returns its id.
func (s *uploadStore) add(name string, r io.Reader) (upload, error) {
	name = filepath.Base(name)
	if name == "." || name == "/" || name == "" {
		name = "file"
	}
	u := upload{ID: newID(), Name: name}
	u.path = filepath.Join(s.dir, u.ID)
	f, err := os.OpenFile(u.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return u, err
	}
	n, err := io.Copy(f, io.LimitReader(r, maxUploadBytes+1))
	f.Close()
	if err == nil && n > maxUploadBytes {
		err = fmt.Errorf("file is larger than %d MB", maxUploadBytes>>20)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil && s.total+n > maxUploadTotal {
		err = fmt.Errorf("uploads exceed %d MB in total; finish or remove some runs first", maxUploadTotal>>20)
	}
	if err != nil {
		os.Remove(u.path)
		return u, err
	}
	u.Size = n
	s.total += n
	s.files[u.ID] = u
	return u, nil
}

// relPath validates a path inside an uploaded folder, as sent by the
// browser. It must stay inside the folder: relative, no "..", no NUL.
func relPath(p string) (string, error) {
	if p == "" || len(p) > maxRelPath || strings.ContainsRune(p, 0) || strings.Contains(p, "\\") {
		return "", fmt.Errorf("invalid path %q", p)
	}
	clean := path.Clean(p)
	if path.IsAbs(clean) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("path %q leaves the folder", p)
	}
	for _, part := range strings.Split(clean, "/") {
		if len(part) > 255 {
			return "", fmt.Errorf("path %q has a name longer than 255 bytes", p)
		}
	}
	return clean, nil
}

// addDir stores a folder sent as a multipart sequence of "path" fields
// each followed by its "file", plus "dir" fields for empty directories.
// Browsers don't send permissions, so files starting with "#!" are made
// executable; everything else is 0644.
func (s *uploadStore) addDir(name string, mr *multipart.Reader) (upload, error) {
	u := upload{ID: newID(), Name: filepath.Base(name), Dir: true}
	if u.Name == "." || u.Name == "/" || u.Name == "" {
		u.Name = "folder"
	}
	u.path = filepath.Join(s.dir, u.ID)
	if err := os.Mkdir(u.path, 0o700); err != nil {
		return u, err
	}
	fail := func(err error) (upload, error) {
		os.RemoveAll(u.path)
		return u, err
	}
	readValue := func(p *multipart.Part) (string, error) {
		b, err := io.ReadAll(io.LimitReader(p, maxRelPath+1))
		if err != nil {
			return "", err
		}
		if len(b) > maxRelPath {
			return "", errors.New("path too long")
		}
		return string(b), nil
	}
	pending := ""
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fail(fmt.Errorf("upload: %w", err))
		}
		switch part.FormName() {
		case "path", "dir":
			v, err := readValue(part)
			if err != nil {
				return fail(err)
			}
			rel, err := relPath(v)
			if err != nil {
				return fail(err)
			}
			if part.FormName() == "dir" {
				if err := os.MkdirAll(filepath.Join(u.path, filepath.FromSlash(rel)), 0o755); err != nil {
					return fail(err)
				}
				continue
			}
			pending = rel
		case "file":
			if pending == "" {
				return fail(errors.New("upload: file without a path"))
			}
			if u.Files >= maxDirFiles {
				return fail(fmt.Errorf("folder has more than %d files", maxDirFiles))
			}
			target := filepath.Join(u.path, filepath.FromSlash(pending))
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return fail(err)
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
			if err != nil {
				return fail(fmt.Errorf("%s: %w", pending, err))
			}
			br := bufio.NewReader(io.LimitReader(part, maxDirUploadBytes-u.Size+1))
			head, _ := br.Peek(2)
			n, err := io.Copy(f, br)
			if err == nil && string(head) == "#!" {
				err = f.Chmod(0o755)
			}
			f.Close()
			if err != nil {
				return fail(err)
			}
			u.Size += n
			if u.Size > maxDirUploadBytes {
				return fail(fmt.Errorf("folder is larger than %d MB", maxDirUploadBytes>>20))
			}
			u.Files++
			pending = ""
		default:
			io.Copy(io.Discard, part)
		}
	}
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

func (s *uploadStore) get(id string) (upload, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.files[id]
	return u, ok
}

func (s *uploadStore) remove(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if u, ok := s.files[id]; ok {
		os.RemoveAll(u.path)
		s.total -= u.Size
		delete(s.files, id)
	}
}

func (s *uploadStore) close() { os.RemoveAll(s.dir) }

// --- Image library: root filesystem tars in one directory. ---

// ImageInfo describes a library image.
type ImageInfo struct {
	Name      string    `json:"name"`
	Size      int64     `json:"size"`
	Modified  time.Time `json:"modified"`
	Cached    bool      `json:"cached"`     // EROFS disk already built
	CacheSize int64     `json:"cache_size"` // its size
	LastUsed  time.Time `json:"last_used,omitzero"`
	Importing bool      `json:"importing"`
	Error     string    `json:"error,omitempty"`
}

type imageLibrary struct {
	dir string
	mu  sync.Mutex
	// imports in progress or failed, by image name
	jobs map[string]*ImageInfo
}

func newImageLibrary(dir string) *imageLibrary {
	return &imageLibrary{dir: dir, jobs: map[string]*ImageInfo{}}
}

var validImageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}\.tar$`)

// path resolves a library image name to its file. Only plain names of
// existing .tar files in the library directory are accepted.
func (l *imageLibrary) path(name string) (string, bool) {
	if !validImageName.MatchString(name) {
		return "", false
	}
	p := filepath.Join(l.dir, name)
	st, err := os.Stat(p)
	return p, err == nil && st.Mode().IsRegular()
}

func (l *imageLibrary) list() []ImageInfo {
	var out []ImageInfo
	entries, _ := os.ReadDir(l.dir)
	seen := map[string]bool{}
	for _, e := range entries {
		if !validImageName.MatchString(e.Name()) {
			continue
		}
		// Stat (not Lstat): symlinking tars into the library is supported.
		info, err := os.Stat(filepath.Join(l.dir, e.Name()))
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		img := ImageInfo{Name: e.Name(), Size: info.Size(), Modified: info.ModTime()}
		if _, c, ok := image.Cached(filepath.Join(l.dir, e.Name())); ok {
			img.Cached, img.CacheSize, img.LastUsed = true, c.Size(), c.ModTime()
		}
		seen[e.Name()] = true
		out = append(out, img)
	}
	l.mu.Lock()
	for name, job := range l.jobs {
		if !seen[name] || job.Importing {
			out = append(out, *job)
		}
	}
	l.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var validRef = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:@-]{0,254}$`)

// importDocker exports a Docker image into the library in the background.
func (l *imageLibrary) importDocker(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if !validRef.MatchString(ref) || strings.Contains(ref, "..") {
		return "", errors.New("not a valid image reference (e.g. alpine:3.20, python:3.12-slim)")
	}
	docker, err := exec.LookPath("docker")
	if err != nil {
		return "", errors.New("docker is not installed")
	}
	name := regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(ref, "_") + ".tar"
	if _, exists := l.path(name); exists {
		return "", fmt.Errorf("%s is already in the library", name)
	}
	if err := os.MkdirAll(l.dir, 0o755); err != nil {
		return "", err
	}
	l.mu.Lock()
	if j, ok := l.jobs[name]; ok && j.Importing {
		l.mu.Unlock()
		return "", fmt.Errorf("%s is already being imported", ref)
	}
	job := &ImageInfo{Name: name, Importing: true, Modified: time.Now()}
	l.jobs[name] = job
	l.mu.Unlock()

	go func() {
		err := exportImage(docker, ref, filepath.Join(l.dir, name))
		l.mu.Lock()
		defer l.mu.Unlock()
		job.Importing = false
		if err != nil {
			job.Error = err.Error()
		} else {
			delete(l.jobs, name)
		}
	}()
	return name, nil
}

func exportImage(docker, ref, out string) error {
	run := func(args ...string) (string, error) {
		b, err := exec.Command(docker, args...).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("docker %s: %v: %s", args[0], err, strings.TrimSpace(string(b)))
		}
		return strings.TrimSpace(string(b)), nil
	}
	if _, err := run("pull", "-q", ref); err != nil {
		return err
	}
	// The container is only created to export its filesystem; it never starts.
	cid, err := run("create", "--entrypoint", "/ksar-export-only", ref)
	if err != nil {
		return err
	}
	defer run("rm", cid)
	tmp := out + ".partial"
	if _, err := run("export", "-o", tmp, cid); err != nil {
		os.Remove(tmp)
		return err
	}
	// Keep what the export drops: environment and working directory.
	if js, err := run("inspect", "--format", "{{json .Config}}", ref); err == nil {
		var c struct {
			Env        []string
			WorkingDir string
		}
		if json.Unmarshal([]byte(js), &c) == nil {
			if err := image.SaveConfig(out, image.Config{Env: c.Env, WorkingDir: c.WorkingDir}); err != nil {
				os.Remove(tmp)
				return err
			}
		}
	}
	return os.Rename(tmp, out)
}

func (l *imageLibrary) remove(name string) error {
	p, ok := l.path(name)
	if !ok {
		l.mu.Lock()
		defer l.mu.Unlock()
		if j, exists := l.jobs[name]; exists && !j.Importing {
			delete(l.jobs, name) // dismiss a failed import
			return nil
		}
		return errors.New("no such image")
	}
	if c, _, cached := image.Cached(p); cached {
		os.Remove(c)
	}
	os.Remove(image.ConfigPath(p))
	return os.Remove(p)
}

// prune removes cached disks that no library image uses.
func (l *imageLibrary) prune() (int64, error) {
	keep := map[string]bool{}
	for _, img := range l.list() {
		if c, _, ok := image.Cached(filepath.Join(l.dir, img.Name)); ok {
			keep[c] = true
		}
	}
	return image.Prune(keep)
}

// --- Settings: defaults for the New run form. ---

// Settings are the user's defaults.
type Settings struct {
	MemMB    int `json:"mem_mb"`
	TimeoutS int `json:"timeout_s"`
}

type settingsStore struct {
	mu   sync.Mutex
	path string
	cur  Settings
}

func newSettingsStore() *settingsStore {
	s := &settingsStore{cur: Settings{MemMB: 256, TimeoutS: 60}}
	if dir, err := os.UserConfigDir(); err == nil {
		s.path = filepath.Join(dir, "ksar", "ui.json")
		if b, err := os.ReadFile(s.path); err == nil {
			var v Settings
			if json.Unmarshal(b, &v) == nil && v.valid() == nil {
				s.cur = v
			}
		}
	}
	return s
}

func (v Settings) valid() error {
	if v.MemMB < minMemMB || v.MemMB > maxMemMB {
		return fmt.Errorf("memory must be between %d and %d MB", minMemMB, maxMemMB)
	}
	if v.TimeoutS < 1 || v.TimeoutS > maxTimeoutS {
		return fmt.Errorf("timeout must be between 1 and %d seconds", maxTimeoutS)
	}
	return nil
}

func (s *settingsStore) get() Settings {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cur
}

func (s *settingsStore) put(v Settings) error {
	if err := v.valid(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cur = v
	if s.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(v, "", "  ")
	return os.WriteFile(s.path, b, 0o600)
}
