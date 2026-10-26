package llm

import (
	"errors"
	"strings"
	"unicode"
)

// Facts is what the assistant is told about a project: a few lines
// gathered from its files, nothing executed.
type Facts struct {
	Language string   // e.g. "Python", from the rule-based detection
	Files    []string // top-level names, folders with a trailing /
	Details  []string // e.g. `package.json scripts: start=node app.js`
}

func (f Facts) describe() string {
	var b strings.Builder
	b.WriteString("Project: " + f.Language + ". Files: " + strings.Join(f.Files, ", ") + ".")
	for _, d := range f.Details {
		b.WriteString(" " + d + ".")
	}
	return b.String()
}

// Worked examples, in exactly the form of the real question: a model this
// small follows patterns far better than instructions.
var examples = []struct{ q, a string }{
	{"Project: Python. Files: requirements.txt, main.py, utils.py.", "sh -c 'pip install -r requirements.txt && python main.py'"},
	{"Project: Node.js. Files: package.json, package-lock.json, src/, test/. package.json scripts: test=jest, build=tsc.", "sh -c 'npm ci && npm test'"},
	{"Project: Go. Files: go.mod, go.sum, main.go, handler.go.", "go run ."},
	{"Project: C. Files: Makefile, hello.c, util.c. Makefile targets: all, run, clean.", "make run"},
	{"Project: Python. Files: pyproject.toml, src/, tests/. pyproject.toml mentions pytest.", "sh -c 'pip install . pytest && python -m pytest -q'"},
	{"Project: Node.js. Files: package.json, server.js. package.json scripts: start=node server.js, dev=nodemon server.js.", "sh -c 'npm install && npm start'"},
	// Projects with several parts run all of them: through a script at
	// the top that drives them, else each from its own folder, a front
	// end's build first.
	{"Project: Python and Node.js. Files: README.md, run.sh, api/, web/. run.sh in the top folder: run.sh runs Python and Node.js. Python in api/: requirements.txt, server.py. Node.js in web/: package.json scripts: build=vite build. the README shows: ./run.sh.", "sh run.sh"},
	{"Project: Go and Node.js. Files: README.md, server/, web/. Go in server/: go.mod, main.go. Node.js in web/: package.json scripts: build=vite build, dev=vite.", "sh -c '(cd web && npm install && npm run build) && (cd server && go run .)'"},
}

const system = "You reply with one shell command that installs a project's dependencies and runs it. " +
	"It must finish by itself: no servers in watch or dev mode. Reply with the command only."

// Prompt builds the chat prompt for a project.
func Prompt(f Facts) string {
	var b strings.Builder
	b.WriteString("<|im_start|>system\n" + system + "<|im_end|>\n")
	for _, e := range examples {
		b.WriteString("<|im_start|>user\n" + e.q + "<|im_end|>\n<|im_start|>assistant\n" + e.a + "<|im_end|>\n")
	}
	b.WriteString("<|im_start|>user\n" + f.describe() + "<|im_end|>\n<|im_start|>assistant\n")
	return b.String()
}

// Clean checks the model's reply: one line, printable, of sane length.
// Whatever it says only ever runs inside the VM, but it must at least be
// a plausible command line.
func Clean(reply string) (string, error) {
	cmd := strings.TrimSpace(reply)
	cmd = strings.Trim(cmd, "`")
	cmd = strings.TrimPrefix(cmd, "$ ")
	if i := strings.IndexByte(cmd, '\n'); i >= 0 {
		cmd = strings.TrimSpace(cmd[:i])
	}
	if cmd == "" || len(cmd) > 400 {
		return "", errors.New("the model's reply isn't a usable command")
	}
	for _, r := range cmd {
		if !unicode.IsPrint(r) {
			return "", errors.New("the model's reply has control characters")
		}
	}
	return cmd, nil
}

// Suggest asks the model for a command.
func (m *Model) Suggest(tok *Tokenizer, f Facts) (string, error) {
	prompt := tok.Encode(Prompt(f))
	nl := tok.Encode("\n")
	out, err := m.Generate(prompt, 60, []int{tok.Special("<|im_end|>"), tok.Special("<|endoftext|>")},
		func(ids []int) bool { return len(nl) == 1 && ids[len(ids)-1] == nl[0] })
	if err != nil {
		return "", err
	}
	return Clean(tok.Decode(out))
}
