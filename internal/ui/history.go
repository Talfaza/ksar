package ui

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Run history on disk: each run is saved as <id>.json.gz in
// Config.HistoryDir (state when it starts, state and output when it ends),
// so finished runs are still there after ksar ui restarts. The directory
// is private (0700, files 0600): output and environment variables can hold
// secrets. Without a HistoryDir, history lives in memory only.

const maxHistoryFile = 16 << 20 // compressed; larger files are skipped on load

var historyName = regexp.MustCompile(`^[0-9a-f]{12}\.json\.gz$`)

type savedRun struct {
	Info RunInfo `json:"info"`
	Log  []Chunk `json:"log,omitempty"`
}

// save writes a run's record, replacing the previous one atomically.
func (m *runManager) save(r *run, withLog bool) {
	if m.cfg.HistoryDir == "" {
		return
	}
	r.mu.Lock()
	rec := savedRun{Info: r.info}
	if withLog {
		rec.Log = append([]Chunk(nil), r.log...)
	}
	r.mu.Unlock()
	if err := writeHistory(m.cfg.HistoryDir, rec); err != nil {
		r.append("ksar", "ksar ui: could not save this run to the history: "+err.Error()+"\n")
	}
}

func writeHistory(dir string, rec savedRun) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".run-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	zw := gzip.NewWriter(f)
	err = json.NewEncoder(zw).Encode(rec)
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, rec.Info.ID+".json.gz"))
}

// loadHistory restores saved runs, newest maxRuns of them. A run saved
// before it ended was cut short when the UI stopped (its process dies with
// the UI), so it is shown as stopped.
func (m *runManager) loadHistory() {
	dir := m.cfg.HistoryDir
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.Type().IsRegular() || !historyName.MatchString(e.Name()) {
			continue
		}
		rec, err := readHistory(filepath.Join(dir, e.Name()))
		if err != nil || rec.Info.ID+".json.gz" != e.Name() {
			continue
		}
		if !finalStatus(rec.Info.Status) {
			rec.Info.Status = "stopped"
			if rec.Info.Ended.IsZero() {
				rec.Info.Ended = rec.Info.Created
			}
			rec.Info.Error = "Ksar UI stopped before this run ended."
		}
		rec.Info.PID, rec.Info.CPUPercent = 0, 0 // no process any more
		r := &run{info: rec.Info, log: rec.Log, size: map[string]int{}, subs: map[chan Chunk]struct{}{},
			done: make(chan struct{})}
		for i := range r.log {
			r.log[i].Seq = i
		}
		close(r.done)
		m.runs[r.info.ID] = r
	}
	for _, id := range m.trimLocked() {
		os.Remove(filepath.Join(dir, id+".json.gz"))
	}
}

func readHistory(path string) (savedRun, error) {
	var rec savedRun
	f, err := os.Open(path)
	if err != nil {
		return rec, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() > maxHistoryFile {
		return rec, errors.New("too large")
	}
	zr, err := gzip.NewReader(f)
	if err != nil {
		return rec, err
	}
	// The log is bounded when written; bound it when read too.
	err = json.NewDecoder(io.LimitReader(zr, 4*maxLogBytes+1<<20)).Decode(&rec)
	return rec, err
}

func finalStatus(s string) bool {
	switch s {
	case "succeeded", "failed", "timed_out", "stopped", "error":
		return true
	}
	return false
}

// forget deletes finished runs from memory and disk: one run by id, or
// every finished run when id is "". It returns how many were deleted.
func (m *runManager) forget(id string) (int, error) {
	m.mu.Lock()
	var ids []string
	for rid, r := range m.runs {
		if (id == "" || rid == id) && r.finished() {
			ids = append(ids, rid)
		}
	}
	if id != "" && len(ids) == 0 {
		_, exists := m.runs[id]
		m.mu.Unlock()
		if exists {
			return 0, errors.New("the run is still going; stop it first")
		}
		return 0, errors.New("no such run")
	}
	for _, rid := range ids {
		delete(m.runs, rid)
	}
	m.mu.Unlock()
	if m.cfg.HistoryDir != "" {
		for _, rid := range ids {
			os.Remove(filepath.Join(m.cfg.HistoryDir, rid+".json.gz"))
		}
	}
	m.bump()
	return len(ids), nil
}

// HistoryInfo describes the saved history for the settings page.
type HistoryInfo struct {
	Dir   string    `json:"dir"` // "" when history isn't saved
	Runs  int       `json:"runs"`
	Bytes int64     `json:"bytes"`
	Since time.Time `json:"since,omitzero"`
}

func (m *runManager) historyInfo() HistoryInfo {
	h := HistoryInfo{Dir: m.cfg.HistoryDir}
	m.mu.Lock()
	for _, r := range m.runs {
		if !r.finished() {
			continue
		}
		h.Runs++
		if h.Since.IsZero() || r.info.Created.Before(h.Since) {
			h.Since = r.info.Created
		}
	}
	m.mu.Unlock()
	if h.Dir != "" {
		entries, _ := os.ReadDir(h.Dir)
		for _, e := range entries {
			if info, err := e.Info(); err == nil && strings.HasSuffix(e.Name(), ".json.gz") {
				h.Bytes += info.Size()
			}
		}
	}
	return h
}
