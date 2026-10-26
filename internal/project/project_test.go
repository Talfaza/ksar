package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestDetectProject(t *testing.T) {
	cases := []struct {
		name    string
		files   map[string]string
		kind    string
		command string
		net     string
	}{
		{"python app", map[string]string{"requirements.txt": "requests\n", "main.py": "print(1)"},
			"python", "sh -c 'pip install -q --disable-pip-version-check --root-user-action=ignore -r requirements.txt && python main.py'", "allow"},
		{"python script", map[string]string{"hello.py": "print(1)"}, "python", "python hello.py", "none"},
		{"python tests", map[string]string{"pyproject.toml": "[project]\nname='x'\n[tool.pytest]\n", "tests/test_a.py": ""},
			"python", "sh -c 'pip install -q --disable-pip-version-check --root-user-action=ignore . && " +
				"pip install -q --disable-pip-version-check --root-user-action=ignore pytest && python -m pytest -q'", "allow"},
		{"node start", map[string]string{"package.json": `{"scripts":{"start":"node s.js"},"dependencies":{"x":"1"}}`, "package-lock.json": "{}"},
			"node", "sh -c 'npm ci --no-audit --no-fund --no-update-notifier && npm start'", "allow"},
		{"node no deps", map[string]string{"package.json": `{"main":"app.js"}`}, "node", "node app.js", "none"},
		{"go main", map[string]string{"go.mod": "module x", "main.go": "package main\nfunc main(){}"}, "go", "go run .", "none"},
		{"go lib", map[string]string{"go.mod": "module x", "go.sum": "", "lib.go": "package lib"}, "go", "go test ./...", "allow"},
		{"rust", map[string]string{"Cargo.toml": "", "src/main.rs": ""}, "rust", "cargo run --quiet", "allow"},
		{"make run", map[string]string{"Makefile": "all:\n\tcc x.c\nrun: all\n\t./a.out\n"}, "make", "make run", "none"},
		{"c", map[string]string{"hello.c": "int main(){}"}, "c", "sh -c 'gcc -O2 -o /tmp/program hello.c && /tmp/program'", "none"},
		{"shell", map[string]string{"run.sh": "echo hi"}, "shell", "sh run.sh", "none"},
		{"unknown", map[string]string{"notes.txt": "x"}, "unknown", "ls -la", "none"},
	}
	for _, c := range cases {
		s := Detect(writeTree(t, c.files))
		if s.Kind != c.kind || s.Command != c.command || s.Net != c.net || s.Docker == "" {
			t.Errorf("%s: got %s / %q / net %s", c.name, s.Kind, s.Command, s.Net)
		}
	}
}

// The project from the bug report: a C program one folder down that uses
// conio.h and opens its data by a path relative to its own folder.
func TestCInSubfolder(t *testing.T) {
	root := writeTree(t, map[string]string{
		"README.md":        "# C-CLI-Animation",
		"Frames/s0.txt":    "frame",
		"animation/main.c": "#include <stdio.h>\n#include <conio.h>\nint main () {\n FILE *p = fopen(\"../Frames/s0.txt\", \"r\"); clrscr(); return 0; }\n",
	})
	p := Analyze(root)
	if p.Kind != "c" || !strings.HasPrefix(p.Command, "sh -c 'cd animation && ") || !strings.Contains(p.Command, "conio.h") ||
		!strings.HasSuffix(p.Command, "-o /tmp/program main.c && /tmp/program'") {
		t.Fatalf("plan = %s / %q", p.Kind, p.Command)
	}
	for _, c := range p.Candidates {
		if !strings.HasPrefix(c, "sh -c 'cd animation && ") {
			t.Errorf("candidate doesn't run in animation/: %q", c)
		}
	}
	details := strings.Join(p.Facts.Details, "; ")
	for _, want := range []string{"animation/", "main.c", "../Frames/s0.txt", "conio.h"} {
		if !strings.Contains(details, want) {
			t.Errorf("facts miss %q: %s", want, details)
		}
	}
	if short := ShortForm(p.Command); short != "sh -c 'cd animation && gcc -O2 -o /tmp/program main.c && /tmp/program'" {
		t.Errorf("short form = %q", short)
	}
}

// A project one folder down (as in many zips and monorepos) runs from
// there.
func TestProjectInSubfolder(t *testing.T) {
	root := writeTree(t, map[string]string{
		"README.md":        "docs",
		"app/package.json": `{"scripts":{"start":"node server.js"},"dependencies":{"x":"1"}}`,
		"app/server.js":    "",
		"docs/index.md":    "",
	})
	p := Analyze(root)
	if p.Kind != "node" || p.Command != "sh -c 'cd app && npm install --no-audit --no-fund --no-update-notifier && npm start'" {
		t.Fatalf("plan = %s / %q", p.Kind, p.Command)
	}
	for _, c := range p.Candidates {
		if !strings.HasPrefix(c, "sh -c 'cd app && ") {
			t.Errorf("candidate doesn't run in app/: %q", c)
		}
	}
	if Analyze(writeTree(t, map[string]string{"notes.txt": ""})).Kind != "unknown" {
		t.Error("a plain folder was recognized")
	}
}

// A project with several runtimes: every component is offered with its
// own environment, and a top-level script that drives them comes first.
func TestMultiRuntime(t *testing.T) {
	root := writeTree(t, map[string]string{
		"README.md":                   "# Shop\n\n```sh\n./run.sh\n```\n",
		"run.sh":                      "cd backend && pip install -r requirements.txt && python app.py &\ncd frontend && npm install && npm start\n",
		"backend/requirements.txt":    "flask",
		"backend/app.py":              "print('api')",
		"backend/utils/helpers.py":    "",
		"frontend/package.json":       `{"scripts":{"start":"node index.js"}}`,
		"frontend/index.js":           "",
		"node_modules/x/package.json": "{}",
	})
	p := Analyze(root)
	if p.Command != "sh run.sh" || p.Docker != "node:22" || p.Net != "allow" {
		t.Fatalf("primary = %q on %s, net %s", p.Command, p.Docker, p.Net)
	}
	for _, h := range []string{"registry.npmjs.org", "pypi.org"} {
		if !strings.Contains(strings.Join(p.Allow, " "), h) {
			t.Errorf("allow misses %s: %v", h, p.Allow)
		}
	}
	if p.Label != "Python + Node.js project" && p.Label != "Node.js + Python project" {
		t.Errorf("label = %q", p.Label)
	}
	byCmd := map[string]Option{}
	for _, o := range p.Options {
		byCmd[o.Command] = o
		if strings.Contains(o.Command, "node_modules") || strings.Contains(o.Command, "utils") {
			t.Errorf("searched a folder it shouldn't: %q", o.Command)
		}
	}
	py := byCmd["sh -c 'cd backend && pip install -q --disable-pip-version-check --root-user-action=ignore -r requirements.txt && python app.py'"]
	js := byCmd["sh -c 'cd frontend && npm install --no-audit --no-fund --no-update-notifier && npm start'"]
	if py.Docker != "python:3.12-slim" || js.Docker != "node:22-slim" {
		t.Errorf("component images: python %q, node %q (options %v)", py.Docker, js.Docker, p.Candidates)
	}
	if len(p.Candidates) != len(p.Options) || len(p.Components) != 3 {
		t.Errorf("%d candidates, %d options, %d components", len(p.Candidates), len(p.Options), len(p.Components))
	}
	facts := strings.Join(p.Facts.Details, "; ")
	for _, want := range []string{"backend/", "frontend/", "the README shows: ./run.sh"} {
		if !strings.Contains(facts, want) {
			t.Errorf("facts miss %q: %s", want, facts)
		}
	}
}

func TestEnvFor(t *testing.T) {
	cases := []struct {
		needs []string
		image string
	}{
		{nil, "alpine:3.20"},
		{[]string{"c"}, "gcc:14"},
		{[]string{"python"}, "python:3.12-slim"},
		{[]string{"python", "c"}, "python:3.12"},
		{[]string{"python", "node"}, "node:22"},
		{[]string{"go"}, "golang:1.24"},
	}
	for _, c := range cases {
		if got := envFor(c.needs).Docker; got != c.image {
			t.Errorf("envFor(%v) = %s, want %s", c.needs, got, c.image)
		}
	}
	if got := toolsIn("python3 -m venv x && npm ci && gcc a.c"); strings.Join(got, ",") != "node,python,c" {
		t.Errorf("toolsIn = %v", got)
	}
}

// Node and Go with nothing at the top to start them: the first option
// runs both (the front end built first) in node:22 with Go installed.
func TestNodeAndGo(t *testing.T) {
	root := writeTree(t, map[string]string{
		"README.md":          "# app",
		"server/go.mod":      "module server\n\ngo 1.22\n",
		"server/main.go":     "package main\nfunc main(){}",
		"web/package.json":   `{"scripts":{"build":"vite build","dev":"vite"},"dependencies":{"vite":"5"}}`,
		"web/src/index.html": "",
	})
	p := Analyze(root)
	want := "(cd web && npm install --no-audit --no-fund --no-update-notifier && npm run build) && (cd server && go run .)"
	if p.Options[0].Component != wholeLabel || !strings.HasSuffix(p.Command, want+"'") || p.Docker != "node:22" {
		t.Fatalf("first option = %q on %s (%s)", p.Command, p.Docker, p.Options[0].Component)
	}
	if !strings.HasPrefix(p.Command, "sh -c 'curl -fsSLo /tmp/go.tgz https://dl.google.com/go/") || !strings.Contains(p.Command, goSHA256) {
		t.Errorf("no Go install: %q", p.Command)
	}
	if p.Options[0].ForModel() != "sh -c '(cd web && npm install && npm run build) && (cd server && go run .)'" {
		t.Errorf("for the model: %q", p.Options[0].ForModel())
	}
	allow := strings.Join(p.Allow, " ")
	for _, h := range []string{"registry.npmjs.org", "proxy.golang.org", "dl.google.com"} {
		if !strings.Contains(allow, h) {
			t.Errorf("allow misses %s: %v", h, p.Allow)
		}
	}
	if p.Note == "" || p.MemMB < 3072 {
		t.Errorf("note %q, mem %d", p.Note, p.MemMB)
	}
	// The single parts are still offered, each in its own image.
	for _, o := range p.Options[1:] {
		if o.Component == "Go in server/" && (!strings.HasPrefix(o.Docker, "golang:") || strings.Contains(o.Command, "dl.google.com")) {
			t.Errorf("Go part: %q on %s", o.Command, o.Docker)
		}
	}
}

// Both in one folder, both run: side by side when neither is a front-end
// build.
func TestNodeAndGoSameFolder(t *testing.T) {
	root := writeTree(t, map[string]string{
		"go.mod":       "module x\n",
		"main.go":      "package main\nfunc main(){}",
		"package.json": `{"scripts":{"start":"node worker.js"}}`,
		"worker.js":    "",
	})
	p := Analyze(root)
	if !strings.HasSuffix(p.Command, "(npm start) & (go run .) & wait'") && !strings.HasSuffix(p.Command, "(go run .) & (npm start) & wait'") {
		t.Fatalf("first option = %q", p.Command)
	}
}

func TestDatabaseNote(t *testing.T) {
	root := writeTree(t, map[string]string{
		"server/go.mod":  "module s\n\nrequire gorm.io/driver/mysql v1.5.7\n",
		"server/main.go": "package main\nfunc main(){}",
	})
	if p := Analyze(root); !strings.Contains(p.Note, "MySQL") {
		t.Errorf("note = %q", p.Note)
	}
}

// A Go API on MySQL whose .env names a local database: the run starts
// MariaDB first with that user and database.
func TestServicesFromProject(t *testing.T) {
	root := writeTree(t, map[string]string{
		"server/go.mod":  "module s\n\nrequire gorm.io/driver/mysql v1.5.7\n",
		"server/main.go": "package main\nfunc main(){}",
		"server/.env":    "PORT=8080\nDNS=shop:s3cret@tcp(127.0.0.1:3306)/shopdb?parseTime=true\n",
	})
	p := Analyze(root)
	o := p.Options[0]
	cmd := unwrap(o.Command)
	if len(o.Services) != 1 || !strings.Contains(cmd, "mariadb") || !strings.Contains(cmd, "'shop'@'localhost' IDENTIFIED BY 's3cret'") ||
		!strings.Contains(cmd, "\\`shopdb\\`") {
		t.Fatalf("command: %s", cmd)
	}
	if o.ForModel() != "sh -c 'cd server && go run .'" {
		t.Errorf("for the model: %q", o.ForModel())
	}
	if !strings.Contains(strings.Join(o.Allow, " "), "dl-cdn.alpinelinux.org") || o.MemMB < 2048 {
		t.Errorf("allow %v, mem %d (image %s)", o.Allow, o.MemMB, o.Docker)
	}
	if !strings.Contains(cmd, `export DATABASE_URL="mysql://shop:s3cret@127.0.0.1:3306/shopdb"`) {
		t.Errorf("no DATABASE_URL: %s", cmd)
	}
}

// Values from the project that could break out of the shell or SQL are
// not used: the defaults are.
func TestServicesRejectUnsafeValues(t *testing.T) {
	for _, env := range []string{
		"DATABASE_URL=postgres://app:x';DROP DATABASE x;--@localhost/app\n",
		"DATABASE_URL=postgres://ap$(id)p:pw@localhost/app\n",
		"DB_USER=a`id`\nDB_NAME=app\nDB_PASSWORD=x\n",
		"DATABASE_URL=postgres://app:pw@db.example.com/app\n", // not local
	} {
		root := writeTree(t, map[string]string{
			"requirements.txt": "psycopg2-binary\n", "app.py": "print(1)", ".env": env,
		})
		cmd := unwrap(Analyze(root).Options[0].Command)
		if !strings.Contains(cmd, `CREATE ROLE \"app\" LOGIN SUPERUSER PASSWORD 'app'`) {
			t.Errorf("%q: %s", env, cmd)
		}
		for _, bad := range []string{"DROP", "$(id)", "`id`", "example.com"} {
			if strings.Contains(cmd, bad) {
				t.Errorf("%q leaked %q into the command", env, bad)
			}
		}
	}
}

func TestComposeServiceName(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json":       `{"scripts":{"start":"node index.js"},"dependencies":{"pg":"8","redis":"4"}}`,
		"index.js":           "",
		"docker-compose.yml": "services:\n  db:\n    environment:\n      POSTGRES_USER: shop\n      POSTGRES_PASSWORD: pw\n      POSTGRES_DB: shop\n  web:\n    environment:\n      DATABASE_URL: postgres://shop:pw@db:5432/shop\n",
	})
	o := Analyze(root).Options[0]
	cmd := unwrap(o.Command)
	if strings.Join(o.Services, ",") != "PostgreSQL,Redis" || !strings.Contains(cmd, "echo '127.0.0.1 db' >> /etc/hosts") ||
		!strings.Contains(cmd, `CREATE ROLE \"shop\"`) || !strings.Contains(cmd, "redis-server") {
		t.Fatalf("%v: %s", o.Services, cmd)
	}
	if !strings.Contains(strings.Join(o.Env, " "), "CI=true") {
		t.Errorf("node env missing: %v", o.Env)
	}
}

func TestNodeAndRust(t *testing.T) {
	root := writeTree(t, map[string]string{
		"engine/Cargo.toml":  "[package]\nname=\"e\"",
		"engine/src/main.rs": "fn main(){}",
		"ui/package.json":    `{"scripts":{"build":"vite build"},"devDependencies":{"vite":"5"}}`,
	})
	o := Analyze(root).Options[0]
	if o.Docker != "rust:1" || !strings.Contains(o.Command, "nodejs.org/dist/"+nodeVersion) || !strings.Contains(o.Command, nodeSHA256) ||
		!strings.Contains(strings.Join(o.Allow, " "), "nodejs.org") {
		t.Fatalf("%s: %s %v", o.Docker, o.Command, o.Allow)
	}
}

func TestNodeGitHooks(t *testing.T) {
	root := writeTree(t, map[string]string{
		"package.json": `{"scripts":{"prepare":"husky install","test":"node test.js"},"devDependencies":{"husky":"8"}}`,
	})
	o := Analyze(root).Options[0]
	if o.Docker != "node:22" || !strings.HasPrefix(o.Command, "sh -c '(git init -q") || !strings.Contains(strings.Join(o.Env, " "), "HUSKY=0") {
		t.Fatalf("%s: %s %v", o.Docker, o.Command, o.Env)
	}
}

func TestRankedKeepsWholeProjectFirst(t *testing.T) {
	root := writeTree(t, map[string]string{
		"api/requirements.txt": "flask", "api/app.py": "",
		"web/package.json": `{"scripts":{"build":"vite build"}}`,
	})
	p := Analyze(root)
	scores := make([]float64, len(p.Options))
	for i := range scores {
		scores[i] = -float64(len(p.Options) - i) // the model likes the last option best
	}
	order := p.Ranked(scores)
	if p.Options[order[0]].Component != wholeLabel || order[1] != len(p.Options)-1 {
		t.Errorf("order %v", order)
	}
}
