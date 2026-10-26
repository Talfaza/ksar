package llm

import (
	"sort"
	"testing"
	"time"
)

func TestRank(t *testing.T) {
	m, tok, _ := testModel(t)
	start := time.Now()
	a, err := NewAssistant(m, tok)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("prefix: %d tokens in %v", a.prefix.pos, time.Since(start).Round(time.Millisecond))
	cases := []struct {
		f     Facts
		cands []string
		want  string
	}{
		{Facts{Language: "Node.js", Files: []string{"package.json", "package-lock.json", "index.js", "test/"},
			Details: []string{"package.json scripts: dev=nodemon index.js, lint=eslint ., test=mocha"}},
			[]string{"sh -c 'npm ci && npm test'", "sh -c 'npm ci && npm run lint'", "sh -c 'npm ci && node index.js'", "npm test", "node index.js"},
			"sh -c 'npm ci && npm test'"},
		{Facts{Language: "Node.js", Files: []string{"package.json", "bot.js"}, Details: []string{"package.json scripts: start=node bot.js"}},
			[]string{"sh -c 'npm install && npm start'", "npm start", "sh -c 'npm install && node bot.js'", "node bot.js"},
			"sh -c 'npm install && npm start'"},
		{Facts{Language: "Python", Files: []string{"requirements.txt", "app.py", "templates/"}},
			[]string{"sh -c 'pip install -r requirements.txt && python app.py'", "python app.py", "sh -c 'pip install -r requirements.txt && python -m pytest -q'", "python -m pytest -q"},
			"sh -c 'pip install -r requirements.txt && python app.py'"},
		{Facts{Language: "Python", Files: []string{"pyproject.toml", "wordcount/", "tests/"}, Details: []string{"pyproject.toml defines the command wordcount", "it uses pytest"}},
			[]string{"sh -c 'pip install . && wordcount'", "sh -c 'pip install . && python -m pytest -q'", "wordcount", "python -m pytest -q", "sh -c 'pip install . && python -m wordcount'"},
			"sh -c 'pip install . && python -m pytest -q'"},
		{Facts{Language: "Go", Files: []string{"go.mod", "cmd/", "internal/"}},
			// No main package at the top, so no "go run ." (project.Analyze).
			[]string{"go test ./...", "go run ./cmd/cli", "go run ./cmd/server"},
			"go test ./..."},
		{Facts{Language: "C", Files: []string{"Makefile", "main.c", "list.c"}, Details: []string{"Makefile targets: all, test"}},
			[]string{"make", "make all", "make test"},
			"make test"},
		{Facts{Language: "Rust", Files: []string{"Cargo.toml", "src/"}},
			[]string{"cargo run --quiet", "cargo test --quiet", "cargo build --quiet"},
			"cargo run --quiet"},
	}
	right := 0
	for _, c := range cases {
		start := time.Now()
		scores, err := a.Rank(c.f, c.cands)
		if err != nil {
			t.Fatal(err)
		}
		idx := make([]int, len(scores))
		for i := range idx {
			idx[i] = i
		}
		sort.Slice(idx, func(x, y int) bool { return scores[idx[x]] > scores[idx[y]] })
		got := c.cands[idx[0]]
		mark := "ok  "
		if got == c.want {
			right++
		} else {
			mark = "MISS"
		}
		t.Logf("%s %v  %-45q (want %q)", mark, time.Since(start).Round(time.Millisecond), got, c.want)
	}
	t.Logf("%d/%d as expected", right, len(cases))
}
