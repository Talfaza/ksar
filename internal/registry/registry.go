// Package registry keeps track of running VMs: each ksar run gets an ID
// and a small state file, so ksar ps can list every VM on this machine
// (started from the CLI or the web UI) and ksar stop can find one.
//
// A running ksar is locked down and can't create or delete files, so it
// opens its entry before lockdown and afterwards only rewrites it in
// place (pwrite into an already-open file). It can't delete it when it
// exits either: readers treat an entry whose process is gone, or whose
// PID now belongs to another process, as stale and remove it.
package registry

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Entry describes one VM.
type Entry struct {
	ID       string    `json:"id"`
	PID      int       `json:"pid"`
	Start    uint64    `json:"start"` // the process start time (/proc/PID/stat), against PID reuse
	Started  time.Time `json:"started"`
	Status   string    `json:"status"` // preparing, booting, running, exited
	ExitCode *int      `json:"exit_code,omitempty"`
	Image    string    `json:"image,omitempty"`
	Command  []string  `json:"command,omitempty"`
	Label    string    `json:"label,omitempty"` // shown instead of the command, e.g. "desktop install"
	MemMB    int       `json:"mem_mb"`
	Display  string    `json:"display,omitempty"`
	VNC      []string  `json:"vnc,omitempty"`
	Net      string    `json:"net,omitempty"`
	Scope    string    `json:"scope,omitempty"`
	CID      uint32    `json:"cid,omitempty"`
}

// recordSize is the fixed size an entry is padded to, so it can be
// rewritten in place without truncating (which lockdown forbids).
const recordSize = 8 << 10

var idPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// ValidID reports whether s can be used as a VM ID.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// NewID returns a random 12-character hex ID.
func NewID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Dir is where entries live: $XDG_RUNTIME_DIR/ksar/vms (private to the
// user and cleared at logout), or a per-user temp directory.
func Dir() (string, error) {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = filepath.Join(os.TempDir(), "ksar-"+strconv.Itoa(os.Getuid()))
	}
	d := filepath.Join(base, "ksar", "vms")
	if err := os.MkdirAll(d, 0o700); err != nil {
		return "", err
	}
	return d, nil
}

// Handle is a VM's own, open entry.
type Handle struct {
	mu sync.Mutex
	f  *os.File
	e  Entry
}

// Register creates the entry for this process. It fails if a live VM
// already has the ID. Call it before lockdown.
func Register(e Entry) (*Handle, error) {
	if !ValidID(e.ID) {
		return nil, fmt.Errorf("invalid VM id %q (lower-case letters, digits and -, up to 32)", e.ID)
	}
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, e.ID+".json")
	// The same process may register again: enterScope re-executes ksar
	// with the same PID and id.
	if old, err := read(path); err == nil && alive(old) && old.PID != os.Getpid() {
		return nil, fmt.Errorf("a VM with id %s is already running (pid %d)", e.ID, old.PID)
	}
	e.PID = os.Getpid()
	e.Start, _ = startTime(e.PID)
	if e.Started.IsZero() {
		e.Started = time.Now()
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	h := &Handle{f: f, e: e}
	if err := h.write(); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	return h, nil
}

// Update changes the entry in place. Safe after lockdown. A nil Handle
// does nothing, so callers need not check whether registration worked.
func (h *Handle) Update(change func(e *Entry)) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	change(&h.e)
	h.write()
}

// Entry returns a copy of the entry.
func (h *Handle) Entry() Entry {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.e
}

func (h *Handle) write() error {
	b, err := json.Marshal(h.e)
	if err != nil {
		return err
	}
	if len(b) >= recordSize {
		// Keep the essentials if a huge command line doesn't fit.
		short := h.e
		cmd := strings.Join(short.Command, " ")
		if len(cmd) > 200 {
			cmd = cmd[:200] + "…"
		}
		short.Command, short.Image = []string{cmd}, filepath.Base(short.Image)
		if b, err = json.Marshal(short); err != nil || len(b) >= recordSize {
			return errors.New("registry entry too large")
		}
	}
	rec := bytes.Repeat([]byte{' '}, recordSize)
	copy(rec, b)
	rec[recordSize-1] = '\n'
	_, err = h.f.WriteAt(rec, 0)
	return err
}

func read(path string) (Entry, error) {
	var e Entry
	b, err := os.ReadFile(path)
	if err != nil {
		return e, err
	}
	if err := json.Unmarshal(bytes.TrimSpace(b), &e); err != nil {
		return e, err
	}
	return e, nil
}

// alive reports whether the entry's process is still the one that wrote it.
func alive(e Entry) bool {
	if e.PID <= 0 {
		return false
	}
	st, err := startTime(e.PID)
	return err == nil && (e.Start == 0 || st == e.Start)
}

// startTime reads field 22 of /proc/PID/stat (clock ticks since boot).
func startTime(pid int) (uint64, error) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0, err
	}
	// The command name (field 2) may contain spaces and parentheses; the
	// fields after it start after the last ')'.
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return 0, errors.New("malformed stat")
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 20 {
		return 0, errors.New("malformed stat")
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// List returns the VMs on this machine, newest first. Entries of
// processes that are gone are removed; those that exited cleanly are
// returned (with Status "exited") only when all is true.
func List(all bool) ([]Entry, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	paths, _ := filepath.Glob(filepath.Join(dir, "*.json"))
	var out []Entry
	for _, p := range paths {
		e, err := read(p)
		if err != nil {
			// Unreadable: mid-rewrite (try once more) or garbage.
			time.Sleep(5 * time.Millisecond)
			if e, err = read(p); err != nil {
				continue
			}
		}
		if !alive(e) {
			e.Status = "exited"
			// Gone: kept an hour after its last update for ps -a, then removed.
			if st, err := os.Stat(p); err == nil && time.Since(st.ModTime()) > time.Hour {
				os.Remove(p)
				continue
			}
			if !all {
				continue
			}
		} else if e.Status == "exited" && !all {
			continue // the process is still winding down
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out, nil
}

// Find returns the live VM whose ID is id or starts with it.
func Find(id string) (Entry, error) {
	list, err := List(false)
	if err != nil {
		return Entry{}, err
	}
	var match []Entry
	for _, e := range list {
		if e.ID == id {
			return e, nil
		}
		if strings.HasPrefix(e.ID, id) {
			match = append(match, e)
		}
	}
	switch len(match) {
	case 0:
		return Entry{}, fmt.Errorf("no running VM with id %s", id)
	case 1:
		return match[0], nil
	}
	return Entry{}, fmt.Errorf("id %s is ambiguous: it matches %d VMs", id, len(match))
}

// Alive reports whether the VM is still running.
func Alive(e Entry) bool { return alive(e) }
