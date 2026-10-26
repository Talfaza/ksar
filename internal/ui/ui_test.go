package ui

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testServer(t *testing.T, ksar string) *Server {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	s, err := New(Config{Addr: "127.0.0.1:0", KsarPath: ksar, ImagesDir: t.TempDir(),
		Status: func() Status { return Status{} }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.ln.Close(); s.up.close() })
	return s
}

func (s *Server) do(method, path, body string, hdr map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = s.host[0]
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	s.http.Handler.ServeHTTP(rec, req)
	return rec
}

func TestRefusesNonLoopback(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "[::]:0", "192.168.1.1:0", "localhost:0"} {
		if s, err := New(Config{Addr: addr}); err == nil {
			s.ln.Close()
			t.Errorf("New(%q) accepted a non-loopback address", addr)
		}
	}
}

func TestSecurityChecks(t *testing.T) {
	s := testServer(t, "/bin/false")
	cookie := s.cookie + "=" + s.token
	auth := map[string]string{"Cookie": cookie}
	post := map[string]string{"Cookie": cookie, "Origin": s.origin, "Content-Type": "application/json"}
	body := `{"command":"true","mem_mb":256,"timeout_s":10}`

	cases := []struct {
		name         string
		method, path string
		body         string
		hdr          map[string]string
		host         string
		want         int
	}{
		{"api without session", "GET", "/api/runs", "", nil, "", 401},
		{"page without session", "GET", "/", "", nil, "", 401},
		{"public stylesheet", "GET", "/app.css", "", nil, "", 200},
		{"wrong token", "GET", "/auth?token=x", "", nil, "", 403},
		{"rebinding Host", "GET", "/api/runs", "", auth, "evil.example:80", 421},
		{"wrong session value", "GET", "/api/runs", "", map[string]string{"Cookie": s.cookie + "=nope"}, "", 401},
		{"session ok", "GET", "/api/runs", "", auth, "", 200},
		{"post without Origin", "POST", "/api/runs", body, map[string]string{"Cookie": cookie, "Content-Type": "application/json"}, "", 403},
		{"post from another local port", "POST", "/api/runs", body,
			map[string]string{"Cookie": cookie, "Origin": "http://127.0.0.1:3000", "Content-Type": "application/json"}, "", 403},
		{"post with null Origin", "POST", "/api/runs", body,
			map[string]string{"Cookie": cookie, "Origin": "null", "Content-Type": "application/json"}, "", 403},
		{"form content type", "POST", "/api/runs", body,
			map[string]string{"Cookie": cookie, "Origin": s.origin, "Content-Type": "text/plain"}, "", 415},
		{"unknown field", "POST", "/api/runs", `{"command":"true","mem_mb":256,"timeout_s":10,"lockdown":false}`, post, "", 400},
		{"image traversal", "POST", "/api/runs", `{"command":"true","image":"../x.tar","mem_mb":256,"timeout_s":10}`, post, "", 400},
		{"delete traversal", "DELETE", "/api/images/..%2fx.tar", "", map[string]string{"Cookie": cookie, "Origin": s.origin}, "", 400},
		{"oversized body", "PUT", "/api/settings", `{"mem_mb":` + strings.Repeat("1", 70000) + `}`, post, "", 400},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(c.body))
		req.Host = s.host[0]
		if c.host != "" {
			req.Host = c.host
		}
		for k, v := range c.hdr {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		s.http.Handler.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s: status %d, want %d (%s)", c.name, rec.Code, c.want, strings.TrimSpace(rec.Body.String()))
		}
		for _, h := range []string{"Content-Security-Policy", "X-Frame-Options", "X-Content-Type-Options"} {
			if rec.Header().Get(h) == "" {
				t.Errorf("%s: missing %s", c.name, h)
			}
		}
	}
}

func TestLoginSetsStrictCookieAndCleansURL(t *testing.T) {
	s := testServer(t, "/bin/false")
	rec := s.do("GET", "/auth?token="+s.token, "", nil)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("login: %d to %q", rec.Code, rec.Header().Get("Location"))
	}
	c := rec.Result().Cookies()
	if len(c) != 1 || !c[0].HttpOnly || c[0].SameSite != http.SameSiteStrictMode || c[0].Value != s.token {
		t.Fatalf("cookie = %+v", c)
	}
	if rec := s.do("POST", "/auth?token="+s.token, "", nil); rec.Code == http.StatusSeeOther {
		t.Error("login accepted POST")
	}
}

func TestSplitCommand(t *testing.T) {
	cases := map[string][]string{
		`echo hi`:                      {"echo", "hi"},
		`  a   b  `:                    {"a", "b"},
		`sh -c 'echo "$HOME"; ls'`:     {"sh", "-c", `echo "$HOME"; ls`},
		`printf "a\"b" x\ y`:           {"printf", `a"b`, "x y"},
		`a''b ""`:                      {"ab", ""},
		`python3 -c "print('hi')"`:     {"python3", "-c", "print('hi')"},
		`echo " hi "; ping google.com`: {"sh", "-c", `echo " hi "; ping google.com`},
		`make && make test`:            {"sh", "-c", `make && make test`},
		`ls *.py`:                      {"sh", "-c", `ls *.py`},
		`cat ~/x > out`:                {"sh", "-c", `cat ~/x > out`},
		`echo 'a;b|c' "$x"`:            {"echo", "a;b|c", "$x"},
		`echo a\;b`:                    {"echo", "a;b"},
		`curl https://x.org/a?b=1`:     {"sh", "-c", `curl https://x.org/a?b=1`},
	}
	for in, want := range cases {
		got, err := splitCommand(in)
		if err != nil || strings.Join(got, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("splitCommand(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{`'open`, `"open`, ``, `   `} {
		if _, err := splitCommand(bad); err == nil {
			t.Errorf("splitCommand(%q) succeeded", bad)
		}
	}
}

func TestImageNames(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ok.tar"), []byte("x"), 0o644)
	os.Mkdir(filepath.Join(dir, "dir.tar"), 0o755)
	lib := newImageLibrary(dir)
	if _, ok := lib.path("ok.tar"); !ok {
		t.Error("ok.tar not found")
	}
	for _, bad := range []string{"../ok.tar", "ok", "/etc/passwd", "dir.tar", ".hidden.tar", "a/b.tar", "missing.tar"} {
		if _, ok := lib.path(bad); ok {
			t.Errorf("path(%q) accepted", bad)
		}
	}
	if _, err := lib.importDocker("UPPER; rm -rf /"); err == nil {
		t.Error("bad docker reference accepted")
	}
}

// A real run through the UI server: needs KVM, the Ksar kernel and the
// Alpine image.
func TestRunThroughServer(t *testing.T) {
	for _, f := range []string{"/dev/kvm", "../../build/bzImage", "../../build/alpine.tar"} {
		if _, err := os.Stat(f); err != nil {
			t.Skipf("%s missing", f)
		}
	}
	bin := filepath.Join(t.TempDir(), "ksar")
	build := exec.Command("make", "-C", "../..", "agent")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("make agent: %v\n%s", err, out)
	}
	build = exec.Command("go", "build", "-o", bin, "ksar/cmd/ksar")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build ksar: %v\n%s", err, out)
	}
	s := testServer(t, bin)
	s.cfg.Kernel, _ = filepath.Abs("../../build/bzImage")
	s.runs.cfg = s.cfg
	alpine, _ := filepath.Abs("../../build/alpine.tar")
	os.Symlink(alpine, filepath.Join(s.cfg.ImagesDir, "alpine.tar"))
	t.Setenv("XDG_CACHE_HOME", t.TempDir())

	r, err := s.runs.start(RunSpec{Image: "alpine.tar", MemMB: 256, TimeoutS: 30,
		Command: `sh -c 'printf "\033]52;c;eA==\007ok \033[31mred\033[0m <i>x</i>\n"; echo e >&2; exit 4'`})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.done:
	case <-time.After(30 * time.Second):
		t.Fatal("run did not finish")
	}
	info := r.snapshot()
	var stdout, stderr string
	for _, c := range r.log {
		switch c.Stream {
		case "stdout":
			stdout += c.Data
		case "stderr":
			stderr += c.Data
		}
	}
	if info.Status != "failed" || info.ExitCode == nil || *info.ExitCode != 4 {
		t.Errorf("status %s exit %v, want failed/4", info.Status, info.ExitCode)
	}
	if stdout != "ok \x1b[31mred\x1b[0m <i>x</i>\n" || stderr != "e\n" {
		t.Errorf("stdout %q stderr %q", stdout, stderr)
	}
	if info.Filtered != 1 || !info.Lockdown || !info.Virtio || info.BootMs == 0 || info.CID == 0 {
		t.Errorf("info = %+v", info)
	}
}

func TestRelPath(t *testing.T) {
	for in, want := range map[string]string{"a.txt": "a.txt", "src/a.go": "src/a.go", "./x/../y": "y", "a//b": "a/b"} {
		if got, err := relPath(in); err != nil || got != want {
			t.Errorf("relPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "/etc/passwd", "../x", "a/../../x", "..", ".", "a\x00b", `a\..\b`, strings.Repeat("x", 300)} {
		if _, err := relPath(bad); err == nil {
			t.Errorf("relPath(%q) accepted", bad)
		}
	}
}

// multipartDir builds a folder upload body: parts are (field, value) pairs.
func multipartDir(t *testing.T, parts [][2]string) (string, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		if p[0] == "file" {
			w, _ := mw.CreateFormFile("file", "f")
			w.Write([]byte(p[1]))
		} else {
			mw.WriteField(p[0], p[1])
		}
	}
	mw.Close()
	return mw.FormDataContentType(), &buf
}

func TestFolderUpload(t *testing.T) {
	s := testServer(t, "/bin/false")
	post := func(parts [][2]string) *httptest.ResponseRecorder {
		ct, body := multipartDir(t, parts)
		return s.do("POST", "/api/uploads/dir?name=proj", body.String(),
			map[string]string{"Cookie": s.cookie + "=" + s.token, "Origin": s.origin, "Content-Type": ct})
	}
	rec := post([][2]string{{"dir", "empty/sub"}, {"path", "src/main.go"}, {"file", "package main"},
		{"path", "run.sh"}, {"file", "#!/bin/sh\necho hi"}})
	if rec.Code != 200 {
		t.Fatalf("upload: %d %s", rec.Code, rec.Body)
	}
	var u upload
	json.Unmarshal(rec.Body.Bytes(), &u)
	stored, _ := s.up.get(u.ID)
	if !u.Dir || u.Files != 2 || u.Name != "proj" {
		t.Errorf("upload = %+v", u)
	}
	if b, _ := os.ReadFile(filepath.Join(stored.path, "src/main.go")); string(b) != "package main" {
		t.Errorf("src/main.go = %q", b)
	}
	if st, err := os.Stat(filepath.Join(stored.path, "run.sh")); err != nil || st.Mode().Perm() != 0o755 {
		t.Errorf("run.sh mode = %v, %v (scripts should be executable)", st.Mode(), err)
	}
	if st, err := os.Stat(filepath.Join(stored.path, "src/main.go")); err != nil || st.Mode().Perm() != 0o644 {
		t.Errorf("main.go mode = %v", st.Mode())
	}
	if st, err := os.Stat(filepath.Join(stored.path, "empty/sub")); err != nil || !st.IsDir() {
		t.Error("empty directory not created")
	}

	for name, parts := range map[string][][2]string{
		"escape with ..":    {{"path", "../../evil"}, {"file", "x"}},
		"absolute path":     {{"path", "/tmp/evil"}, {"file", "x"}},
		"file without path": {{"file", "x"}},
		"duplicate file":    {{"path", "a"}, {"file", "1"}, {"path", "a"}, {"file", "2"}},
		"dir escape":        {{"dir", "../outside"}},
	} {
		if rec := post(parts); rec.Code != 400 {
			t.Errorf("%s: status %d, want 400", name, rec.Code)
		}
	}
	// Nothing may have been written next to the upload directories.
	entries, _ := os.ReadDir(filepath.Dir(stored.path))
	for _, e := range entries {
		if !e.IsDir() {
			t.Errorf("stray file %s outside any upload", e.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(stored.path), "outside")); err == nil {
		t.Error("dir escape created a directory outside the upload")
	}
}
