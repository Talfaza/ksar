package ui

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Finished runs come back after a restart, cut-short ones as stopped, and
// deleting removes them from disk.
func TestRunHistory(t *testing.T) {
	dir := t.TempDir()
	code := 0
	done := savedRun{Info: RunInfo{ID: "aaaaaaaaaaaa", Status: "succeeded", ExitCode: &code,
		Created: time.Now().Add(-time.Minute), Spec: RunSpec{Command: "echo hi"}},
		Log: []Chunk{{Seq: 0, Stream: "stdout", Data: "hi\n"}}}
	cut := savedRun{Info: RunInfo{ID: "bbbbbbbbbbbb", Status: "running", Created: time.Now()}}
	for _, rec := range []savedRun{done, cut} {
		if err := writeHistory(dir, rec); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(dir, "../evil.json.gz"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "cccccccccccc.json.gz"), []byte("not gzip"), 0o600)
	if st, _ := os.Stat(filepath.Join(dir, "aaaaaaaaaaaa.json.gz")); st.Mode().Perm() != 0o600 {
		t.Errorf("history file mode %v", st.Mode().Perm())
	}

	m := newRunManager(Config{HistoryDir: dir}, nil, nil)
	runs := m.list()
	if len(runs) != 2 {
		t.Fatalf("%d runs restored", len(runs))
	}
	r, _ := m.get("aaaaaaaaaaaa")
	backlog, ch := r.subscribe(-1)
	if len(backlog) != 1 || backlog[0].Data != "hi\n" {
		t.Errorf("log = %+v", backlog)
	}
	if _, open := <-ch; open {
		t.Error("subscription to a restored run stays open")
	}
	if c, _ := m.get("bbbbbbbbbbbb"); c.info.Status != "stopped" || c.info.Error == "" {
		t.Errorf("cut-short run: %s %q", c.info.Status, c.info.Error)
	}
	if h := m.historyInfo(); h.Runs != 2 || h.Bytes == 0 {
		t.Errorf("history info %+v", h)
	}

	if _, err := m.forget("aaaaaaaaaaaa"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "aaaaaaaaaaaa.json.gz")); !os.IsNotExist(err) {
		t.Error("deleted run still on disk")
	}
	if n, _ := m.forget(""); n != 1 || len(m.list()) != 0 {
		t.Errorf("clear: %d deleted, %d left", n, len(m.list()))
	}
}
