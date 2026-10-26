package project

import (
	"bufio"
	"io/fs"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Projects often hold several runtimes: a Node front end and a Python API,
// a Go service with Python scripts, a C program next to a shell script
// that drives everything. Analyze finds each of them (a component: a
// folder and a kind), proposes commands for each, and describes the whole
// tree, so the model chooses among all of them with the full picture.

// Option is one way to run the project: a command and the environment it
// needs (each component has its own image and registries).
type Option struct {
	Command   string   `json:"command"`
	Component string   `json:"component"` // e.g. "Python in backend/"
	Docker    string   `json:"docker"`
	Image     string   `json:"image,omitempty"` // library name, when already imported
	Net       string   `json:"net"`
	Allow     []string `json:"allow,omitempty"`
	MemMB     int      `json:"mem_mb"`
	TimeoutS  int      `json:"timeout_s"`
	Env       []string `json:"env,omitempty"`
	Services  []string `json:"services,omitempty"` // e.g. "PostgreSQL", started in the VM first
	Note      string   `json:"note,omitempty"`
	// Core is the project's own command when Command adds setup steps in
	// front of it (installing a runtime or starting services): what the
	// page shows, and, without noise flags, what the model judges.
	Core string   `json:"core,omitempty"`
	dirs []string // the project folders it runs
}

// Ranked orders a plan's options by the model's scores, best first. A
// project with several parts runs all of them unless the user picks one:
// the whole-project option stays first (a model this small prefers one
// part's short command), and the scores order the rest.
func (p Plan) Ranked(scores []float64) []int {
	order := make([]int, len(p.Options))
	for i := range order {
		order[i] = i
	}
	pinned := func(i int) bool { return p.Options[i].Component == wholeLabel }
	sort.SliceStable(order, func(a, b int) bool {
		if pa, pb := pinned(order[a]), pinned(order[b]); pa != pb {
			return pa
		}
		return scores[order[a]] > scores[order[b]]
	})
	return order
}

// ForModel is the command as the model should judge it: without setup
// steps (runtime and service installs) or noise flags.
func (o Option) ForModel() string {
	if o.Core != "" {
		return ShortForm(o.Core)
	}
	return ShortForm(o.Command)
}

// Component is one runtime found in the project.
type Component struct {
	Dir   string `json:"dir"` // "." for the top
	Kind  string `json:"kind"`
	Label string `json:"label"` // e.g. "Python in backend/"
}

// Plan is a project's suggestion (its first option), every option, and the
// facts a model is given to choose among them.
type Plan struct {
	Suggestion
	Components []Component `json:"components"`
	Options    []Option    `json:"options"`
	Candidates []string    `json:"candidates"` // the options' commands
	Facts      Facts       `json:"-"`
}

const (
	maxComponents = 8
	maxOptions    = 16
	perComponent  = 4 // options per component before the rest are added
)

// skipDirs are never searched for components.
var skipDirs = map[string]bool{"node_modules": true, "vendor": true, "venv": true, ".venv": true, "__pycache__": true,
	"target": true, "dist": true, "build": true, "out": true, "site-packages": true}

// Analyze describes the whole project and every way it could be run.
func Analyze(root string) Plan {
	var comps []Component
	var plans []Plan
	filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.IsDir() || len(comps) >= maxComponents {
			return nil
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if rel != "." {
			if strings.HasPrefix(d.Name(), ".") || skipDirs[d.Name()] || strings.Count(rel, "/") >= 3 {
				return fs.SkipDir
			}
		}
		files := projectFiles(p)
		for _, k := range kinds(p, files, true) {
			// Below the top, a loose .py file isn't a project; a manifest
			// or an entry point is.
			if k == "python" && rel != "." && !pythonEntry(files) {
				continue
			}
			kp := analyzeKind(p, k)
			c := Component{Dir: rel, Kind: k, Label: languageName(k) + " in " + where(rel)}
			if k == "make" || k == "shell" {
				c.Label = kp.Reasons[0] + " in " + where(rel)
			}
			comps = append(comps, c)
			plans = append(plans, kp)
		}
		return nil
	})
	if len(comps) == 0 {
		sug := Detect(root)
		return Plan{Suggestion: sug, Candidates: []string{sug.Command},
			Options: []Option{optionFrom(sug, sug.Command, "", ".")}, Facts: Facts{Language: "unknown", Files: topNames(root)}}
	}

	// The likely main one first: a top-level script or Makefile that ties
	// several runtimes together, else the shallowest component.
	order := make([]int, len(comps))
	for i := range order {
		order[i] = i
	}
	langs := languages(comps)
	sort.SliceStable(order, func(a, b int) bool {
		ca, cb := comps[order[a]], comps[order[b]]
		oa, ob := isOrchestrator(ca, len(langs)), isOrchestrator(cb, len(langs))
		if oa != ob {
			return oa
		}
		return strings.Count(ca.Dir, "/")+depth(ca.Dir) < strings.Count(cb.Dir, "/")+depth(cb.Dir)
	})

	var p Plan
	seen := map[string]bool{}
	add := func(o Option) {
		if len(p.Options) < maxOptions && !seen[o.Command] {
			seen[o.Command] = true
			p.Options = append(p.Options, o)
		}
	}
	// Several runtimes and nothing at the top that starts them: offer
	// running every part, first.
	whole := len(langs) > 1 && !isOrchestrator(comps[order[0]], len(langs))
	if whole {
		add(wholeProject(comps, plans, order))
	}
	// Each component's best few first, so every runtime is represented;
	// then the rest, while there is room.
	for pass := 0; pass < 2; pass++ {
		for _, i := range order {
			cands := plans[i].Candidates
			if pass == 0 {
				cands = cands[:min(perComponent, len(cands))]
			}
			for _, c := range cands {
				add(optionFrom(plans[i].Suggestion, c, comps[i].Label, comps[i].Dir))
			}
		}
	}
	for i := range p.Options {
		p.Options[i] = withServices(root, p.Options[i], p.Options[i].dirs)
		if p.Options[i].Core == p.Options[i].Command {
			p.Options[i].Core = "" // nothing in front of it
		}
	}
	for _, i := range order {
		p.Components = append(p.Components, comps[i])
	}
	main := plans[order[0]]
	p.Suggestion = main.Suggestion
	o := p.Options[0]
	p.Command, p.Docker, p.Net, p.Allow, p.MemMB, p.TimeoutS, p.Env, p.Note = o.Command, o.Docker, o.Net, o.Allow, o.MemMB, o.TimeoutS, o.Env, o.Note
	if whole {
		p.Kind = "mixed"
	}
	if len(langs) > 1 {
		p.Label = strings.Join(langs, " + ") + " project"
		p.Reasons = nil
		for _, i := range order {
			p.Reasons = append(p.Reasons, comps[i].Label)
		}
	} else if comps[order[0]].Dir != "." {
		p.Reasons = append([]string{comps[order[0]].Dir + "/"}, p.Reasons...)
	}
	for _, o := range p.Options {
		p.Candidates = append(p.Candidates, o.Command)
	}

	// What the model is told: the tree's top, each component with its own
	// details, and how the README says to run it.
	p.Facts = Facts{Language: strings.Join(langs, " and "), Files: topNames(root)}
	for _, i := range order {
		line := comps[i].Label
		if d := plans[i].Facts.Details; len(d) > 0 {
			line += ": " + strings.Join(d, "; ")
		} else if len(plans[i].Reasons) > 0 {
			line += ": " + strings.Join(plans[i].Reasons, ", ")
		}
		p.Facts.Details = append(p.Facts.Details, line)
	}
	if len(o.Services) > 0 {
		p.Facts.Details = append(p.Facts.Details, "it uses "+strings.Join(o.Services, " and "))
	}
	if hints := readmeCommands(root); len(hints) > 0 {
		p.Facts.Details = append(p.Facts.Details, "the README shows: "+strings.Join(hints, "; "))
	}
	return p
}

func optionFrom(s Suggestion, cmd, component, dir string) Option {
	if dir != "." && dir != "" {
		cmd = inDir(dir, cmd)
	}
	return Option{Command: withSetup(s.setup, cmd), Component: component, Docker: s.Docker, Net: s.Net, Allow: s.Allow,
		MemMB: s.MemMB, TimeoutS: s.TimeoutS, Env: s.Env, Note: s.Note, Core: cmd, dirs: []string{dir}}
}

const wholeLabel = "The whole project"

// wholeProject runs every part of a project with several runtimes, each
// from its own folder, in one image that has all of them. A Node part
// with a build script next to another runtime is a front end: it is
// installed and built first. The rest then run; if there are several
// (an API and a worker, say) they run side by side.
func wholeProject(comps []Component, plans []Plan, order []int) Option {
	var builds, runs, needs, dirs []string
	others := map[string]bool{}
	for _, i := range order {
		if k := comps[i].Kind; k != "make" && k != "shell" {
			others[k] = true
		}
	}
	for _, i := range order {
		c, kp := comps[i], plans[i]
		if c.Kind == "make" || c.Kind == "shell" {
			continue
		}
		needs = append(needs, c.Kind)
		dirs = append(dirs, c.Dir)
		in := func(cmd string) string {
			if c.Dir == "." {
				return "(" + unwrap(cmd) + ")"
			}
			return "(cd " + c.Dir + " && " + unwrap(cmd) + ")"
		}
		if c.Kind == "node" && len(others) > 1 {
			if b := withSuffix(kp.Candidates, "npm run build"); b != "" {
				builds = append(builds, in(b))
				continue
			}
		}
		runs = append(runs, in(kp.Command))
	}
	script := strings.Join(builds, " && ")
	switch run := strings.Join(runs, " & "); {
	case len(runs) > 1 && script != "":
		script += " && { " + run + " & wait; }"
	case len(runs) > 1:
		script = run + " & wait"
	case len(runs) == 1 && script != "":
		script += " && " + run
	case len(runs) == 1:
		script = run
	}
	env := envFor(needs)
	core := "sh -c " + shQuote(script)
	return Option{Command: withSetup(env.setup, core), Component: wholeLabel, Docker: env.Docker,
		Net: env.Net, Allow: env.Allow, MemMB: env.MemMB, TimeoutS: env.TimeoutS, Env: env.Env, Note: env.Note, Core: core, dirs: dirs}
}

// withSuffix is the first command ending in suffix (with its install
// step, when there is one: those come first).
func withSuffix(cmds []string, suffix string) string {
	for _, c := range cmds {
		if strings.HasSuffix(strings.TrimSuffix(c, "'"), suffix) {
			return c
		}
	}
	return ""
}

// unwrap is the script inside a "sh -c '...'" command.
func unwrap(cmd string) string {
	if strings.HasPrefix(cmd, "sh -c '") && strings.HasSuffix(cmd, "'") {
		return strings.ReplaceAll(cmd[len("sh -c '"):len(cmd)-1], `'\''`, "'")
	}
	return cmd
}

// withSetup puts a runtime install in front of a command.
func withSetup(setup, cmd string) string {
	if setup == "" || strings.Contains(cmd, setup) {
		return cmd
	}
	return "sh -c " + shQuote(setup+" && "+unwrap(cmd))
}

func where(dir string) string {
	if dir == "." {
		return "the top folder"
	}
	return dir + "/"
}

func depth(dir string) int {
	if dir == "." {
		return 0
	}
	return 1
}

// languages lists the distinct runtimes, in a stable order.
func languages(comps []Component) []string {
	var out []string
	seen := map[string]bool{}
	for _, c := range comps {
		if c.Kind == "make" || c.Kind == "shell" {
			continue
		}
		n := languageName(c.Kind)
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	if len(out) == 0 {
		out = []string{languageName(comps[0].Kind)}
	}
	return out
}

func isOrchestrator(c Component, languages int) bool {
	return c.Dir == "." && (c.Kind == "shell" || c.Kind == "make") && languages > 1
}

// pythonEntry reports whether a folder has a Python manifest or entry point.
func pythonEntry(files map[string]bool) bool {
	return files["requirements.txt"] || files["pyproject.toml"] || files["setup.py"] ||
		first(files, "main.py", "app.py", "run.py", "manage.py", "__main__.py") != ""
}

// --- environments for commands that use several runtimes ---

var toolKinds = []struct {
	re   *regexp.Regexp
	kind string
}{
	{regexp.MustCompile(`\b(npm|npx|node|yarn|pnpm)\b`), "node"},
	{regexp.MustCompile(`\b(python3?|pip3?|pytest|uvicorn|gunicorn|flask)\b`), "python"},
	{regexp.MustCompile(`\bgo (run|build|test)\b`), "go"},
	{regexp.MustCompile(`\b(cargo|rustc)\b`), "rust"},
	{regexp.MustCompile(`\b(gcc|g\+\+|cc|clang|make)\b`), "c"},
}

// toolsIn lists the runtimes a script or Makefile calls.
func toolsIn(text string) []string {
	var out []string
	for _, t := range toolKinds {
		if t.re.MatchString(text) {
			out = append(out, t.kind)
		}
	}
	return out
}

// toolNames names the runtimes a script calls, for the model.
func toolNames(text string) string {
	var names []string
	for _, k := range toolsIn(text) {
		names = append(names, languageName(k))
	}
	return strings.Join(names, " and ")
}

var registries = map[string][]string{
	"node":   {"registry.npmjs.org"},
	"python": {"pypi.org", "files.pythonhosted.org"},
	"go":     {"proxy.golang.org", "sum.golang.org", "storage.googleapis.com"},
	"rust":   {"crates.io", "index.crates.io", "static.crates.io"},
}

// envFor picks one image with every runtime needed. The full (non-slim)
// official images are built on Debian's buildpack-deps, so node:22,
// python:3.12, golang:1.24 and rust:1 all include gcc, make and Python 3.
func envFor(needs []string) Suggestion {
	has := map[string]bool{}
	for _, n := range needs {
		has[n] = true
	}
	s := Suggestion{Net: "none", MemMB: 512, TimeoutS: 300}
	switch {
	case has["node"]:
		s.Docker = "node:22"
	case has["go"]:
		s.Docker = "golang:1.24"
	case has["rust"]:
		s.Docker = "rust:1"
	case has["python"] && has["c"]:
		s.Docker = "python:3.12"
	case has["python"]:
		s.Docker = "python:3.12-slim"
	case has["c"]:
		s.Docker = "gcc:14"
	default:
		s.Docker = "alpine:3.20"
		s.MemMB, s.TimeoutS = 256, 120
	}
	for _, k := range []string{"node", "python", "go", "rust"} {
		if has[k] {
			s.Net = "allow"
			s.Allow = append(s.Allow, registries[k]...)
			s.MemMB, s.TimeoutS = max(s.MemMB, 1024), 900
		}
	}
	if has["go"] || has["rust"] {
		s.MemMB = 2048
	}
	for _, k := range []string{"node", "python"} {
		if has[k] {
			s.Env = append(s.Env, kindEnv(k)...)
		}
	}
	if has["node"] && has["rust"] {
		// No official image has both: rust:1 gets Node's release archive,
		// checked against its published SHA-256 before it is unpacked.
		s.Docker, s.setup = "rust:1", nodeSetup
		s.Allow = append(s.Allow, "nodejs.org")
		s.MemMB = 3072
		s.Note = "Needs Node.js and Rust: this uses rust:1 and first installs Node.js " + nodeVersion + " from nodejs.org (checked by SHA-256)."
	}
	if has["node"] && has["go"] {
		// No official image has both: node:22 gets Go's release archive,
		// checked against its published SHA-256 before it is unpacked.
		s.setup = goSetup
		s.Allow = append(s.Allow, "dl.google.com")
		s.MemMB = 3072
		s.Note = "Needs Node.js and Go: this uses node:22 and first installs " + goVersion + " from dl.google.com (checked by SHA-256)."
	}
	return s
}

// kindEnv is the environment a language's tools run best with here:
// non-interactive (no watch modes, no prompts), output not held back, and
// no git hooks installed (there is no git repository to hook into).
func kindEnv(kind string) []string {
	switch kind {
	case "node":
		return []string{"CI=true", "HUSKY=0", "HUSKY_SKIP_INSTALL=1"}
	case "python":
		return []string{"PYTHONUNBUFFERED=1", "PIP_NO_INPUT=1"}
	}
	return nil
}

// The Node.js release installed into images without Node.
const (
	nodeVersion = "v22.23.4"
	nodeSHA256  = "b8492284c76a54baf58d61668dd0e41ee0fc98adabf9503d30f05d08f55b4406"
	nodeSetup   = "curl -fsSLo /tmp/node.tgz https://nodejs.org/dist/" + nodeVersion + "/node-" + nodeVersion + "-linux-x64.tar.gz" +
		" && echo \"" + nodeSHA256 + "  /tmp/node.tgz\" | sha256sum -c --quiet && tar -C /usr/local --strip-components=1 -xzf /tmp/node.tgz && rm /tmp/node.tgz"
)

// The Go release installed into images without Go (Ksar guests are x86-64).
const (
	goVersion = "go1.27.3"
	goSHA256  = "65efbc80fdc1fa2cd2631cad5b481777a68e8725f34e89f50fc6ac714e379b40"
	goSetup   = "curl -fsSLo /tmp/go.tgz https://dl.google.com/go/" + goVersion + ".linux-amd64.tar.gz" +
		" && echo \"" + goSHA256 + "  /tmp/go.tgz\" | sha256sum -c --quiet && tar -C /usr/local -xzf /tmp/go.tgz && rm /tmp/go.tgz" +
		" && export PATH=/usr/local/go/bin:$PATH GOPATH=/tmp/gopath GOCACHE=/tmp/gocache"
)

func (s *Suggestion) setEnv(e Suggestion) {
	s.Docker, s.Net, s.Allow, s.MemMB, s.TimeoutS, s.setup, s.Env = e.Docker, e.Net, e.Allow, e.MemMB, e.TimeoutS, e.setup, e.Env
	if e.Note != "" {
		s.Note = e.Note
	}
}

// --- what the README says ---

var readmeCommand = regexp.MustCompile(`^\$?\s*((npm|npx|yarn|pnpm|node|python3?|pip3?|go|cargo|make|gcc|g\+\+|sh|bash|\./)\S*\b.*)$`)

// readmeCommands pulls up to five command lines out of the README's code
// blocks (or "$ " lines). They are facts for the model, never options:
// README text is the project's, and only Ksar builds what can run.
func readmeCommands(root string) []string {
	name := first(projectFiles(root), "README.md", "README", "readme.md", "README.rst", "README.txt")
	if name == "" {
		return nil
	}
	var out []string
	inCode := false
	sc := bufio.NewScanner(strings.NewReader(string(readSmall(root, name))))
	for sc.Scan() && len(out) < 5 {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "```") {
			inCode = !inCode
			continue
		}
		if !inCode && !strings.HasPrefix(l, "$ ") {
			continue
		}
		if m := readmeCommand.FindStringSubmatch(l); m != nil && len(m[1]) <= 100 {
			out = append(out, m[1])
		}
	}
	return out
}
