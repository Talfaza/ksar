// Package project recognizes what kind of software project a folder holds
// and how to run it: an image, the network its dependency install needs,
// and a short list of plausible commands. It only reads file names and
// small manifests; nothing from the project runs.
package project

import (
	"encoding/json"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Detection: given a folder, suggest how to run it in a
// VM: an image, a command that installs dependencies and starts (or tests)
// it, and the network it needs for the install, limited to the language's
// package registry. It only reads file names and a few small manifests;
// nothing from the project runs on the host.

// Suggestion is a proposed run for a project.
type Suggestion struct {
	Kind     string   `json:"kind"`    // python, node, go, rust, c, make, shell, unknown
	Label    string   `json:"label"`   // e.g. "Python project"
	Reasons  []string `json:"reasons"` // what it was recognized by
	Docker   string   `json:"docker"`  // image to use, as a Docker reference
	Image    string   `json:"image"`   // its library name; "" if it must be imported
	Command  string   `json:"command"`
	Net      string   `json:"net"` // none or allow
	Allow    []string `json:"allow,omitempty"`
	MemMB    int      `json:"mem_mb"`
	TimeoutS int      `json:"timeout_s"`
	Note     string   `json:"note,omitempty"`
	Env      []string `json:"env,omitempty"`
	setup    string   // installs a runtime the image lacks (see envFor)
}

// LibraryName is the name an imported Docker image gets in the library
// (see importDocker).
func LibraryName(ref string) string {
	return regexp.MustCompile(`[^A-Za-z0-9._-]+`).ReplaceAllString(ref, "_") + ".tar"
}

// projectFiles lists a folder's files (relative, slash separated), up to
// a few levels deep: enough to recognize a project, cheap on big ones.
func projectFiles(root string) map[string]bool {
	files := map[string]bool{}
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			name := d.Name()
			if rel != "." && (strings.Count(rel, "/") >= 3 || name == "node_modules" || name == ".git" ||
				name == "target" || name == "__pycache__" || name == ".venv" || name == "venv") {
				return fs.SkipDir
			}
			return nil
		}
		if len(files) < 5000 {
			files[rel] = true
		}
		return nil
	})
	return files
}

func readSmall(root, rel string) []byte {
	p := filepath.Join(root, filepath.FromSlash(rel))
	if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() || st.Size() > 1<<20 {
		return nil
	}
	b, _ := os.ReadFile(p)
	return b
}

func matching(files map[string]bool, pred func(string) bool) []string {
	var out []string
	for f := range files {
		if pred(f) {
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

func first(files map[string]bool, names ...string) string {
	for _, n := range names {
		if files[n] {
			return n
		}
	}
	return ""
}

// Detect looks at a folder and suggests a run for its main kind (see
// kinds for the order); Analyze looks at the whole tree.
func Detect(root string) Suggestion {
	files := projectFiles(root)
	if ks := kinds(root, files, false); len(ks) > 0 {
		return suggestFor(root, files, ks[0])
	}
	return Suggestion{Kind: "unknown", Label: "Folder", Docker: "alpine:3.20", Net: "none", MemMB: 256, TimeoutS: 60,
		Command: "ls -la", Note: "Not a project Ksar recognizes; enter the command to run."}
}

// kinds lists what a folder is, most specific first. With localC, a C
// program counts only if its sources are in this folder itself.
func kinds(root string, files map[string]bool, localC bool) []string {
	topLevel := func(ext string) []string {
		return matching(files, func(f string) bool { return !strings.Contains(f, "/") && strings.HasSuffix(f, ext) })
	}
	var out []string
	if files["package.json"] {
		out = append(out, "node")
	}
	if files["pyproject.toml"] || files["requirements.txt"] || files["setup.py"] || len(topLevel(".py")) > 0 {
		out = append(out, "python")
	}
	if files["go.mod"] {
		out = append(out, "go")
	}
	if files["Cargo.toml"] {
		out = append(out, "rust")
	}
	if files["Makefile"] || files["makefile"] {
		out = append(out, "make")
	}
	if c := findC(root, files); c != nil && (!localC || c.dir == ".") {
		out = append(out, "c")
	}
	if first(files, "run.sh", "main.sh", "start.sh", "test.sh") != "" {
		out = append(out, "shell")
	}
	return out
}

// suggestFor suggests a run of one kind found in a folder.
func suggestFor(root string, files map[string]bool, kind string) Suggestion {
	topLevel := func(ext string) []string {
		return matching(files, func(f string) bool { return !strings.Contains(f, "/") && strings.HasSuffix(f, ext) })
	}
	switch kind {
	case "node":
		return nodeProject(root, files)
	case "python":
		return pythonProject(root, files, topLevel(".py"))
	case "go":
		return goProject(root, files)
	case "rust":
		s := Suggestion{Kind: "rust", Label: "Rust project", Reasons: []string{"Cargo.toml"}, Docker: "rust:1-slim",
			Net: "allow", Allow: []string{"crates.io", "index.crates.io", "static.crates.io"}, MemMB: 2048, TimeoutS: 900}
		if files["src/main.rs"] {
			s.Command = "cargo run --quiet"
		} else {
			s.Command = "cargo test --quiet"
			s.Note = "No src/main.rs, so this runs the tests."
		}
		return s
	case "make":
		mkName := first(files, "Makefile", "makefile")
		mk := string(readSmall(root, mkName))
		s := Suggestion{Kind: "make", Label: "Make project", Reasons: []string{mkName}, Command: "make"}
		for _, target := range []string{"run", "test", "check"} {
			if regexp.MustCompile(`(?m)^` + target + `\s*:`).MatchString(mk) {
				s.Command = "make " + target
				s.Reasons = append(s.Reasons, "a "+target+" target")
				break
			}
		}
		s.setEnv(envFor(append([]string{"c"}, toolsIn(mk)...)))
		return s
	case "c":
		c := findC(root, files)
		reasons := []string{path.Join(c.dir, c.main)}
		note := ""
		if c.conio {
			reasons = append(reasons, "conio.h")
			note = "Uses <conio.h> (Windows/Turbo C); Ksar compiles it with a small compatibility shim (clrscr, getch)."
		}
		return Suggestion{Kind: "c", Label: "C/C++ program", Reasons: reasons, Docker: "gcc:14", Net: "none",
			MemMB: 512, TimeoutS: 300, Command: c.command(c.srcs, true), Note: note}
	case "shell":
		sh := first(files, "run.sh", "main.sh", "start.sh", "test.sh")
		s := Suggestion{Kind: "shell", Label: "Shell script", Reasons: []string{sh}, Command: "sh " + sh}
		s.setEnv(envFor(toolsIn(string(readSmall(root, sh)))))
		return s
	}
	return Suggestion{Kind: "unknown"}
}

func pythonProject(root string, files map[string]bool, pyFiles []string) Suggestion {
	s := Suggestion{Kind: "python", Label: "Python project", Docker: "python:3.12-slim",
		Net: "none", MemMB: 1024, TimeoutS: 600, Env: kindEnv("python")}
	var steps []string
	const pip = "pip install -q --disable-pip-version-check --root-user-action=ignore "
	reqs := string(readSmall(root, "requirements.txt"))
	pyproject := string(readSmall(root, "pyproject.toml"))
	switch {
	case files["requirements.txt"]:
		s.Reasons = append(s.Reasons, "requirements.txt")
		if strings.TrimSpace(reqs) != "" {
			steps = append(steps, pip+"-r requirements.txt")
		}
	case files["pyproject.toml"] || files["setup.py"]:
		s.Reasons = append(s.Reasons, first(files, "pyproject.toml", "setup.py"))
		steps = append(steps, pip+".")
	}
	usesPytest := strings.Contains(reqs, "pytest") || strings.Contains(pyproject, "pytest") ||
		files["pytest.ini"] || files["conftest.py"] || files["tests/conftest.py"]
	entry := first(files, "main.py", "app.py", "run.py", "__main__.py")
	if entry == "" && len(pyFiles) == 1 {
		entry = pyFiles[0]
	}
	switch {
	case entry != "":
		steps = append(steps, "python "+entry)
		s.Reasons = append(s.Reasons, entry)
	case usesPytest || len(matching(files, func(f string) bool { return strings.HasPrefix(f, "tests/") || strings.HasPrefix(f, "test_") })) > 0:
		if !strings.Contains(reqs, "pytest") {
			steps = append(steps, pip+"pytest")
		}
		steps = append(steps, "python -m pytest -q")
		s.Reasons = append(s.Reasons, "tests")
	default:
		steps = append(steps, "python -c 'print(\"no entry point found: edit the command\")'")
		s.Note = "No main.py, app.py or tests found: edit the command."
	}
	if len(steps) > 1 || strings.HasPrefix(steps[0], "pip") {
		s.Net, s.Allow = "allow", []string{"pypi.org", "files.pythonhosted.org"}
	}
	s.Command = shellJoin(steps)
	return s
}

// toolConfig matches build tools' config files (vite.config.js,
// .eslintrc.js, ...), which are never what runs a project.
var toolConfig = regexp.MustCompile(`(^\.|\.config\.[cm]?js$|rc\.[cm]?js$)`)

func nodeProject(root string, files map[string]bool) Suggestion {
	s := Suggestion{Kind: "node", Label: "Node.js project", Reasons: []string{"package.json"}, Docker: "node:22-slim",
		MemMB: 1024, TimeoutS: 600}
	var pkg struct {
		Main         string            `json:"main"`
		Scripts      map[string]string `json:"scripts"`
		Dependencies map[string]string `json:"dependencies"`
		DevDeps      map[string]string `json:"devDependencies"`
	}
	json.Unmarshal(readSmall(root, "package.json"), &pkg)
	s.Env = kindEnv("node")
	// Git hooks and scripts that call git (husky, lint-staged, "git
	// describe" in a build) need git and a repository: the full image has
	// git, and the run starts with an empty one.
	usesGit := false
	for _, d := range []string{"husky", "lint-staged", "simple-git-hooks", "pre-commit", "yorkie"} {
		_, dev := pkg.DevDeps[d]
		_, dep := pkg.Dependencies[d]
		usesGit = usesGit || dev || dep
	}
	for _, cmd := range pkg.Scripts {
		usesGit = usesGit || regexp.MustCompile(`\bgit\s`).MatchString(cmd)
	}
	if usesGit {
		s.Docker = "node:22"
		s.setup = "(git init -q >/dev/null 2>&1; true)"
		s.Reasons = append(s.Reasons, "git hooks")
	}
	var steps []string
	if len(pkg.Dependencies)+len(pkg.DevDeps) > 0 {
		if files["package-lock.json"] {
			steps = append(steps, "npm ci --no-audit --no-fund --no-update-notifier")
			s.Reasons = append(s.Reasons, "package-lock.json")
		} else {
			steps = append(steps, "npm install --no-audit --no-fund --no-update-notifier")
		}
		s.Net, s.Allow = "allow", []string{"registry.npmjs.org"}
	} else {
		s.Net = "none"
	}
	switch {
	case pkg.Scripts["start"] != "":
		steps = append(steps, "npm start")
		s.Reasons = append(s.Reasons, "a start script")
	case pkg.Scripts["test"] != "" && !strings.Contains(pkg.Scripts["test"], "no test specified"):
		steps = append(steps, "npm test")
		s.Reasons = append(s.Reasons, "a test script")
	case pkg.Main != "":
		steps = append(steps, "node "+pkg.Main)
	case first(files, "index.js", "main.js", "app.js", "server.js") != "":
		steps = append(steps, "node "+first(files, "index.js", "main.js", "app.js", "server.js"))
	case pkg.Scripts["build"] != "":
		// A front end (Vite and the like): building it is what finishes.
		steps = append(steps, "npm run build")
		s.Reasons = append(s.Reasons, "a build script")
	default:
		steps = append(steps, "node index.js")
	}
	s.Command = shellJoin(steps)
	return s
}

func goProject(root string, files map[string]bool) Suggestion {
	s := Suggestion{Kind: "go", Label: "Go module", Reasons: []string{"go.mod"}, Docker: "golang:1.24-alpine",
		MemMB: 2048, TimeoutS: 900, Net: "none"}
	if files["go.sum"] {
		s.Net, s.Allow = "allow", []string{"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"}
	}
	mainHere := false
	for f := range files {
		if !strings.Contains(f, "/") && strings.HasSuffix(f, ".go") && !strings.HasSuffix(f, "_test.go") {
			if regexp.MustCompile(`(?m)^package main\b`).Match(readSmall(root, f)) {
				mainHere = true
				break
			}
		}
	}
	if mainHere {
		s.Command = "go run ."
		s.Reasons = append(s.Reasons, "package main")
	} else {
		s.Command = "go test ./..."
		s.Note = "No main package at the top, so this runs the tests."
	}
	return s
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

// shellJoin runs steps in order, stopping at the first failure.
func shellJoin(steps []string) string {
	if len(steps) == 1 {
		return steps[0]
	}
	return "sh -c " + shQuote(strings.Join(steps, " && "))
}

func shQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Facts are what a model is told about the project: its language, its
// top-level names, and a few details from its manifests.
type Facts struct {
	Language string
	Files    []string
	Details  []string
}

const maxCandidates = 12 // per component

// inDir makes a command run from a subfolder.
func inDir(dir, cmd string) string {
	if strings.HasPrefix(cmd, "sh -c '") && strings.HasSuffix(cmd, "'") {
		return "sh -c 'cd " + dir + " && " + cmd[len("sh -c '"):]
	}
	return "sh -c " + shQuote("cd "+dir+" && "+cmd)
}

func analyzeKind(root, kind string) Plan {
	files := projectFiles(root)
	sug := suggestFor(root, files, kind)
	p := Plan{Suggestion: sug, Facts: Facts{Language: languageName(sug.Kind), Files: topNames(root)}}
	add := func(c string) {
		if c == "" || len(p.Candidates) >= maxCandidates {
			return
		}
		for _, have := range p.Candidates {
			if have == c {
				return
			}
		}
		p.Candidates = append(p.Candidates, c)
	}
	add(sug.Command)
	combine := func(installs, runs []string) {
		for _, r := range runs {
			for _, i := range installs {
				if i == "" {
					add(r)
				} else {
					add(shellJoin([]string{i, r}))
				}
			}
		}
	}
	topLevel := func(ext string) []string {
		return matching(files, func(f string) bool { return !strings.Contains(f, "/") && strings.HasSuffix(f, ext) })
	}

	switch sug.Kind {
	case "python":
		const pip = "pip install -q --disable-pip-version-check --root-user-action=ignore "
		installs := []string{""}
		if files["requirements.txt"] {
			installs = []string{pip + "-r requirements.txt", ""}
		} else if files["pyproject.toml"] || files["setup.py"] {
			installs = []string{pip + ".", ""}
		}
		var runs []string
		for _, f := range topLevel(".py") {
			if !strings.HasPrefix(f, "test_") && f != "setup.py" && f != "conftest.py" {
				runs = append(runs, "python "+f)
			}
		}
		for f := range files {
			if strings.HasSuffix(f, "/__main__.py") && strings.Count(f, "/") == 1 {
				runs = append(runs, "python -m "+strings.TrimSuffix(f, "/__main__.py"))
			}
		}
		pyproject := string(readSmall(root, "pyproject.toml"))
		for _, name := range consoleScripts(pyproject) {
			runs = append(runs, name)
			p.Facts.Details = append(p.Facts.Details, "pyproject.toml defines the command "+name)
		}
		runs = append(runs, "python -m pytest -q")
		if strings.Contains(pyproject, "pytest") || strings.Contains(string(readSmall(root, "requirements.txt")), "pytest") {
			p.Facts.Details = append(p.Facts.Details, "it uses pytest")
		}
		combine(installs, runs)
	case "node":
		var pkg struct {
			Main    string            `json:"main"`
			Scripts map[string]string `json:"scripts"`
		}
		json.Unmarshal(readSmall(root, "package.json"), &pkg)
		installs := []string{"npm install --no-audit --no-fund --no-update-notifier", ""}
		if files["package-lock.json"] {
			installs = []string{"npm ci --no-audit --no-fund --no-update-notifier", ""}
		}
		var runs, scripts []string
		names := make([]string, 0, len(pkg.Scripts))
		for name := range pkg.Scripts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			cmd := pkg.Scripts[name]
			scripts = append(scripts, name+"="+cmd)
			if watches(name, cmd) {
				continue // never finishes on its own
			}
			switch name {
			case "start", "test":
				runs = append(runs, "npm "+name)
			default:
				runs = append(runs, "npm run "+name)
			}
		}
		if len(scripts) > 0 {
			p.Facts.Details = append(p.Facts.Details, "package.json scripts: "+strings.Join(scripts, ", "))
		}
		if pkg.Main != "" {
			runs = append(runs, "node "+pkg.Main)
		}
		for _, f := range topLevel(".js") {
			if !toolConfig.MatchString(f) {
				runs = append(runs, "node "+f)
			}
		}
		combine(installs, runs)
	case "go":
		// "go run ." only with a main package at the top (Detect checked).
		var runs []string
		if sug.Command == "go run ." {
			runs = append(runs, "go run .")
		}
		runs = append(runs, "go test ./...")
		var cmds []string
		seen := map[string]bool{}
		for f := range files {
			if d := path.Dir(f); strings.HasPrefix(f, "cmd/") && strings.Count(f, "/") == 2 && strings.HasSuffix(f, ".go") && !seen[d] {
				seen[d] = true
				cmds = append(cmds, "go run ./"+d)
			}
		}
		sort.Strings(cmds)
		combine([]string{""}, append(runs, cmds...))
	case "rust":
		combine([]string{""}, []string{"cargo run --quiet", "cargo test --quiet", "cargo build --quiet"})
	case "make":
		mk := string(readSmall(root, first(files, "Makefile", "makefile")))
		targets := makeTargets(mk)
		if len(targets) > 0 {
			p.Facts.Details = append(p.Facts.Details, "Makefile targets: "+strings.Join(targets, ", "))
		}
		runs := []string{"make"}
		for _, t := range targets {
			runs = append(runs, "make "+t)
		}
		combine([]string{""}, runs)
	case "c":
		if c := findC(root, files); c != nil {
			// From its own folder (relative paths in the code) or from the
			// top; all of the folder's sources, or only the one with main.
			add(c.command(c.srcs, true))
			add(c.command([]string{c.main}, true))
			if c.dir != "." {
				add(c.command(c.srcs, false))
			}
			where := "the top folder"
			if c.dir != "." {
				where = c.dir + "/"
			}
			p.Facts.Details = append(p.Facts.Details, "C sources in "+where+": "+strings.Join(c.srcs, ", "),
				"main() is in "+c.main)
			if c.relative != "" {
				p.Facts.Details = append(p.Facts.Details, c.main+" opens files by relative path, like "+c.relative)
			}
			if c.conio {
				p.Facts.Details = append(p.Facts.Details, "it uses conio.h, which Ksar provides")
			}
		}
	case "shell":
		for _, f := range topLevel(".sh") {
			add("sh " + f)
			if uses := toolNames(string(readSmall(root, f))); uses != "" {
				p.Facts.Details = append(p.Facts.Details, f+" runs "+uses)
			}
		}
	}
	if sug.Kind == "make" {
		if uses := toolNames(string(readSmall(root, first(files, "Makefile", "makefile")))); uses != "" {
			p.Facts.Details = append(p.Facts.Details, "the Makefile runs "+uses)
		}
	}
	return p
}

func languageName(kind string) string {
	switch kind {
	case "python":
		return "Python"
	case "node":
		return "Node.js"
	case "go":
		return "Go"
	case "rust":
		return "Rust"
	case "make", "c":
		return "C"
	case "shell":
		return "shell"
	}
	return "unknown"
}

// watches reports whether an npm script runs until stopped.
func watches(name, cmd string) bool {
	if name == "dev" || name == "watch" || strings.HasPrefix(name, "serve") {
		return true
	}
	for _, w := range []string{"nodemon", "--watch", " -w ", "webpack serve", "vite dev", "next dev"} {
		if strings.Contains(cmd, w) {
			return true
		}
	}
	return false
}

// consoleScripts lists [project.scripts] names in a pyproject.toml.
func consoleScripts(pyproject string) []string {
	var out []string
	in := false
	for _, l := range strings.Split(pyproject, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "[") {
			in = l == "[project.scripts]"
			continue
		}
		if name, _, ok := strings.Cut(l, "="); in && ok {
			if n := strings.Trim(strings.TrimSpace(name), `"'`); regexp.MustCompile(`^[A-Za-z0-9._-]+$`).MatchString(n) {
				out = append(out, n)
			}
		}
	}
	return out
}

// makeTargets lists a Makefile's explicit targets (not variables or
// pattern rules), in order.
func makeTargets(mk string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^([A-Za-z0-9][A-Za-z0-9_.-]*)\s*:([^=]|$)`).FindAllStringSubmatch(mk, -1) {
		if t := m[1]; !seen[t] && !strings.HasPrefix(t, ".") && t != "clean" && t != "install" && len(out) < 8 {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// topNames lists the folder's top-level entries (folders end in /),
// skipping hidden ones, at most 25.
func topNames(root string) []string {
	entries, _ := os.ReadDir(root)
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || len(out) >= 25 {
			continue
		}
		if e.IsDir() {
			out = append(out, e.Name()+"/")
		} else {
			out = append(out, e.Name())
		}
	}
	return out
}

// cProgram is a C or C++ program found in a project.
type cProgram struct {
	dir      string   // folder holding the sources, relative ("." for the top)
	srcs     []string // the folder's source files
	main     string   // the one defining main
	cxx      bool
	conio    bool   // includes <conio.h> (Windows/Turbo C)
	math     bool   // includes <math.h>: needs -lm
	relative string // a relative path the code opens, e.g. "../Frames/s0.txt"
}

var (
	cMain     = regexp.MustCompile(`(?m)^\s*(int|void)?\s*main\s*\(`)
	cRelative = regexp.MustCompile(`fopen\s*\(\s*"(\.\.?/[^"]+)"`)
)

// findC looks up to three folders deep for C/C++ sources and picks the
// folder whose sources define main.
func findC(root string, files map[string]bool) *cProgram {
	byDir := map[string][]string{}
	for f := range files {
		switch path.Ext(f) {
		case ".c", ".cpp", ".cc", ".cxx":
			byDir[path.Dir(f)] = append(byDir[path.Dir(f)], path.Base(f))
		}
	}
	dirs := make([]string, 0, len(byDir))
	for d := range byDir {
		dirs = append(dirs, d)
	}
	// Shallowest first, so a top-level program wins over examples below it.
	sort.Slice(dirs, func(i, j int) bool {
		if di, dj := strings.Count(dirs[i], "/"), strings.Count(dirs[j], "/"); di != dj || dirs[i] == "." || dirs[j] == "." {
			return dirs[i] == "." || di < dj && dirs[j] != "."
		}
		return dirs[i] < dirs[j]
	})
	for _, d := range dirs {
		srcs := byDir[d]
		sort.Strings(srcs)
		c := &cProgram{dir: d, srcs: srcs}
		for _, s := range srcs {
			code := string(readSmall(root, path.Join(d, s)))
			if c.main == "" && cMain.MatchString(code) {
				c.main = s
			}
			if path.Ext(s) != ".c" {
				c.cxx = true
			}
			c.conio = c.conio || strings.Contains(code, "<conio.h>")
			c.math = c.math || strings.Contains(code, "<math.h>")
			if m := cRelative.FindStringSubmatch(code); m != nil && c.relative == "" {
				c.relative = m[1]
			}
		}
		if c.main != "" {
			return c
		}
	}
	return nil
}

// command compiles srcs and runs the program, from the sources' folder
// (inFolder) or from the top. With conio.h, an empty header and two macros
// stand in for the Windows console functions it is usually used for.
func (c *cProgram) command(srcs []string, inFolder bool) string {
	cc := "gcc"
	if c.cxx {
		cc = "g++"
	}
	var steps []string
	files := srcs
	if inFolder && c.dir != "." {
		steps = append(steps, "cd "+c.dir)
	} else if c.dir != "." {
		files = nil
		for _, s := range srcs {
			files = append(files, path.Join(c.dir, s))
		}
	}
	flags := "-O2"
	if c.conio {
		steps = append(steps, "mkdir -p /tmp/ksar-compat", "touch /tmp/ksar-compat/conio.h")
		flags += ` -w -I/tmp/ksar-compat -include unistd.h -include stdio.h -D"clrscr()=printf(\"\033[2J\033[H\")" -D"getch()=getchar()"`
	}
	libs := ""
	if c.math {
		libs = " -lm"
	}
	steps = append(steps, cc+" "+flags+" -o /tmp/program "+strings.Join(files, " ")+libs, "/tmp/program")
	return shellJoin(steps)
}

// noise is what ShortForm removes: flags that only quiet tools and the
// conio.h shim, which say nothing about which command is the right one.
var noise = regexp.MustCompile(` -q --disable-pip-version-check --root-user-action=ignore| --no-audit --no-fund --no-update-notifier| --quiet|` +
	`mkdir -p /tmp/ksar-compat && touch /tmp/ksar-compat/conio\.h && | -w -I/tmp/ksar-compat -include unistd\.h -include stdio\.h -D"clrscr\(\)=printf\(\\"[^"]*\\"\)" -D"getch\(\)=getchar\(\)"`)

// ShortForm is a command as a model should judge it: without noise.
func ShortForm(cmd string) string { return noise.ReplaceAllString(cmd, "") }
