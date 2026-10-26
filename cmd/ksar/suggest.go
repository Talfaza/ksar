package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"ksar/internal/llm"
	"ksar/internal/project"
)

// cmdSuggest prints how Ksar would run a project folder: the rule-based
// suggestion, and the built-in model's ranking of the candidate commands.
func cmdSuggest(args []string) int {
	fs := flag.NewFlagSet("suggest", flag.ExitOnError)
	download := fs.Bool("download", false, "download the assistant model if it isn't installed ("+humanBytes(llm.DownloadSize())+", once)")
	asJSON := fs.Bool("json", false, "print JSON")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, `Usage: ksar suggest [-download] [-json] DIR

Suggests how to run the project in DIR: an image, the network its
dependency install needs, and a command. The command is chosen by a tiny
language model (`+llm.ModelName+`) that runs on this machine's CPU, from
candidates built from the project's files; without the model, by rules.
Nothing in DIR is executed.

Flags:
`)
		fs.PrintDefaults()
	}
	fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	dir := fs.Arg(0)
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		fmt.Fprintf(os.Stderr, "ksar: %s is not a directory\n", dir)
		return 1
	}
	plan := project.Analyze(dir)
	type choice struct {
		project.Option
		Score float64 `json:"score"`
	}
	var choices []choice
	var modelErr error
	took := time.Duration(0)
	if !llm.Installed() && *download {
		fmt.Fprintf(os.Stderr, "ksar: downloading %s (%s)…\n", llm.ModelName, humanBytes(llm.DownloadSize()))
		last := time.Now()
		modelErr = llm.Download(context.Background(), func(done, total int64) {
			if time.Since(last) > time.Second {
				fmt.Fprintf(os.Stderr, "\r  %d%%", done*100/total)
				last = time.Now()
			}
		})
		fmt.Fprintln(os.Stderr)
	}
	if modelErr == nil && llm.Installed() && len(plan.Options) > 1 {
		start := time.Now()
		var a *llm.Assistant
		if a, modelErr = llm.LoadAssistant(); modelErr == nil {
			short := make([]string, len(plan.Options))
			for i, o := range plan.Options {
				short[i] = o.ForModel()
			}
			var scores []float64
			if scores, modelErr = a.Rank(llm.Facts{Language: plan.Facts.Language, Files: plan.Facts.Files, Details: plan.Facts.Details}, short); modelErr == nil {
				for _, i := range plan.Ranked(scores) {
					choices = append(choices, choice{plan.Options[i], scores[i]})
				}
			}
		}
		took = time.Since(start)
	}
	// The pick, with the environment of the part of the project it runs.
	pick, by := plan.Options[0], "rules"
	if len(choices) > 0 {
		pick, by = choices[0].Option, llm.ModelName
	}
	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]any{"kind": plan.Kind, "components": plan.Components, "image": pick.Docker, "net": pick.Net,
			"allow": pick.Allow, "mem_mb": pick.MemMB, "timeout_s": pick.TimeoutS, "env": pick.Env, "services": pick.Services, "command": pick.Command, "chosen_by": by,
			"choices": choices, "options": plan.Options})
		return 0
	}
	fmt.Printf("%s (%s)\n", plan.Label, strings.Join(plan.Reasons, ", "))
	fmt.Printf("  image    %s\n  network  %s %s\n  command  %s\n", pick.Docker, pick.Net, strings.Join(pick.Allow, ","), pick.Command)
	if len(pick.Services) > 0 {
		fmt.Printf("  services %s\n", strings.Join(pick.Services, ", "))
	}
	if plan.Note != "" {
		fmt.Printf("  note     %s\n", plan.Note)
	}
	switch {
	case len(choices) > 0:
		fmt.Printf("\nchosen by %s in %v from %d candidates:\n", llm.ModelName, took.Round(time.Millisecond), len(choices))
		for _, c := range choices {
			fmt.Printf("  %7.3f  %-17s %s\n", c.Score, c.Docker, c.Command)
		}
	case modelErr != nil:
		fmt.Printf("\n(the model failed: %v; this is the rule-based choice)\n", modelErr)
	case !llm.Installed():
		fmt.Printf("\n(chosen by rules; run with -download to let the built-in model choose)\n")
	}
	fmt.Printf("\nrun it:\n  ksar run -image <%s> -copy %s:/work/project -w /work/project", pick.Docker, dir)
	fmt.Printf(" -mem %dM -timeout %ds", pick.MemMB, pick.TimeoutS)
	if pick.Net == "allow" {
		fmt.Printf(" -net allow -allow %s", strings.Join(pick.Allow, ","))
	}
	for _, e := range pick.Env {
		fmt.Printf(" -e %s", shellQuote(e))
	}
	fmt.Printf(" -- %s\n", pick.Command)
	return 0
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }
