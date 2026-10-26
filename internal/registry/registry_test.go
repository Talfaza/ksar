package registry

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRegistry(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	h, err := Register(Entry{ID: "abc123", Status: "booting", Command: []string{"sleep", "1"}})
	if err != nil {
		t.Fatal(err)
	}
	// The same process may register the id again (ksar re-executes itself).
	if _, err := Register(Entry{ID: "abc123"}); err != nil {
		t.Errorf("re-register from the same process: %v", err)
	}
	h.Update(func(e *Entry) { e.Status = "running"; e.CID = 7 })
	list, err := List(false)
	if err != nil || len(list) != 1 || list[0].Status != "running" || list[0].CID != 7 {
		t.Fatalf("list = %+v, %v", list, err)
	}
	if e, err := Find("abc"); err != nil || e.ID != "abc123" {
		t.Errorf("prefix find: %+v %v", e, err)
	}
	if _, err := Find("zzz"); err == nil {
		t.Error("found a VM that doesn't exist")
	}

	// In-place updates never leave a longer old record's tail behind.
	h.Update(func(e *Entry) { e.Command = []string{strings.Repeat("x", 3000)} })
	h.Update(func(e *Entry) { e.Command = []string{"short"} })
	if list, _ := List(false); len(list) != 1 || list[0].Command[0] != "short" {
		t.Errorf("after shrinking: %+v", list)
	}
	// Too big for a record: still written, command shortened.
	h.Update(func(e *Entry) { e.Command = []string{strings.Repeat("y", 20000)} })
	if list, _ := List(false); len(list) != 1 || len(list[0].Command[0]) > 300 {
		t.Errorf("oversized entry not shortened")
	}
}

func TestStaleEntries(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	vms, _ := Dir()
	write := func(name, body string, age time.Duration) {
		p := filepath.Join(vms, name+".json")
		os.WriteFile(p, []byte(body), 0o600)
		os.Chtimes(p, time.Now().Add(-age), time.Now().Add(-age))
	}
	// A dead PID, our PID with a wrong start time (PID reuse), and garbage.
	write("dead", `{"id":"dead","pid":999999999,"status":"running","started":"2026-01-01T00:00:00Z"}`, time.Minute)
	write("reused", `{"id":"reused","pid":`+itoa(os.Getpid())+`,"start":1,"status":"running"}`, time.Minute)
	write("old", `{"id":"old","pid":999999998,"status":"exited"}`, 2*time.Hour)
	write("junk", `not json`, time.Minute)
	if list, _ := List(false); len(list) != 0 {
		t.Errorf("stale entries listed as running: %+v", list)
	}
	all, _ := List(true)
	ids := map[string]string{}
	for _, e := range all {
		ids[e.ID] = e.Status
	}
	if ids["dead"] != "exited" || ids["reused"] != "exited" || ids["old"] != "" {
		t.Errorf("ps -a = %v", ids)
	}
	if _, err := os.Stat(filepath.Join(vms, "old.json")); !os.IsNotExist(err) {
		t.Error("entry gone for over an hour was not removed")
	}
	// A dead VM's id can be reused.
	if _, err := Register(Entry{ID: "dead"}); err != nil {
		t.Errorf("reusing a dead VM's id: %v", err)
	}
}

func TestValidID(t *testing.T) {
	for id, ok := range map[string]bool{"a5648b9e980c": true, "web-test": true, "../x": false, "": false,
		"UPPER": false, "-x": false, strings.Repeat("a", 33): false} {
		if ValidID(id) != ok {
			t.Errorf("ValidID(%q) = %v", id, !ok)
		}
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
