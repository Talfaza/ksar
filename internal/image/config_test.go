package image

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConfigRoundTripAndValidation(t *testing.T) {
	tar := filepath.Join(t.TempDir(), "img.tar")
	if c, err := LoadConfig(tar); err != nil || len(c.Env) != 0 {
		t.Fatalf("missing config: %+v, %v", c, err)
	}
	want := Config{Env: []string{"PATH=/usr/local/go/bin:/usr/bin", "GOPATH=/go"}, WorkingDir: "/src"}
	if err := SaveConfig(tar, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(tar)
	if err != nil || strings.Join(got.Env, ",") != strings.Join(want.Env, ",") || got.WorkingDir != "/src" {
		t.Fatalf("round trip: %+v, %v", got, err)
	}
	for _, bad := range []string{`{"env":["NOEQUALS"]}`, `{"env":["=x"]}`, `{"workdir":"relative"}`, `not json`,
		`{"env":["` + strings.Repeat("A", 2<<20) + `=1"]}`} {
		os.WriteFile(ConfigPath(tar), []byte(bad), 0o644)
		if _, err := LoadConfig(tar); err == nil {
			t.Errorf("accepted bad config %.40q", bad)
		}
	}
}
