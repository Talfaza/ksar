package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLookPathUsesGivenPATH(t *testing.T) {
	dir := t.TempDir()
	tool := filepath.Join(dir, "mytool")
	os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755)
	os.WriteFile(filepath.Join(dir, "notexec"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(dir, "adir"), 0o755)

	if p, err := lookPath("mytool", "/nonexistent:"+dir); err != nil || p != tool {
		t.Errorf("lookPath = %q, %v", p, err)
	}
	for _, name := range []string{"notexec", "adir", "missing"} {
		if _, err := lookPath(name, dir); err == nil {
			t.Errorf("lookPath(%q) found something", name)
		}
	}
	if p, _ := lookPath("./rel/prog", ""); p != "./rel/prog" {
		t.Errorf("names with a slash must be used as given, got %q", p)
	}
}

func TestEnvValueLastWins(t *testing.T) {
	env := []string{"PATH=/a", "X=1", "PATH=/b"}
	if v := envValue(env, "PATH"); v != "/b" {
		t.Errorf("PATH = %q", v)
	}
	if v := envValue(env, "NOPE"); v != "" {
		t.Errorf("NOPE = %q", v)
	}
}
