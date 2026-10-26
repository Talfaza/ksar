package ui

import (
	"context"
	"errors"
	"sync"
	"time"

	"ksar/internal/llm"
	"ksar/internal/project"
)

// The command assistant: a tiny language model running in this process
// (package llm) that picks how to run an uploaded project from the
// commands project.Analyze proposes. It only ever chooses from that list:
// text inside a project can't make it suggest anything else, and the
// network policy never comes from it.

const assistantIdle = 10 * time.Minute

// AssistantStatus is the assistant's state for the browser.
type AssistantStatus struct {
	State string `json:"state"` // absent, downloading, ready, error
	Model string `json:"model"`
	Size  int64  `json:"size"` // download size
	Done  int64  `json:"done,omitempty"`
	Error string `json:"error,omitempty"`
}

type assistant struct {
	mu          sync.Mutex
	downloading bool
	done        int64
	err         string
	a           *llm.Assistant
	loading     chan struct{} // closed when a load in progress finishes
	loadErr     error
	lastUse     time.Time
	cancel      context.CancelFunc
}

func (as *assistant) status() AssistantStatus {
	as.mu.Lock()
	defer as.mu.Unlock()
	st := AssistantStatus{Model: llm.ModelName, Size: llm.DownloadSize(), State: "absent"}
	switch {
	case as.downloading:
		st.State, st.Done = "downloading", as.done
	case llm.Installed():
		st.State = "ready"
	case as.err != "":
		st.State, st.Error = "error", as.err
	}
	return st
}

// download fetches the model in the background.
func (as *assistant) download() {
	as.mu.Lock()
	if as.downloading || llm.Installed() {
		as.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	as.downloading, as.done, as.err, as.cancel = true, 0, "", cancel
	as.mu.Unlock()
	go func() {
		err := llm.Download(ctx, func(done, total int64) {
			as.mu.Lock()
			as.done = done
			as.mu.Unlock()
		})
		as.mu.Lock()
		as.downloading = false
		if err != nil {
			as.err = err.Error()
		}
		as.mu.Unlock()
	}()
}

func (as *assistant) remove() error {
	as.mu.Lock()
	if as.cancel != nil {
		as.cancel()
	}
	as.a, as.err = nil, ""
	as.mu.Unlock()
	return llm.Remove()
}

// get returns the loaded model, loading it if needed. Concurrent callers
// share one load.
func (as *assistant) get() (*llm.Assistant, error) {
	as.mu.Lock()
	if as.a != nil {
		as.lastUse = time.Now()
		a := as.a
		as.mu.Unlock()
		return a, nil
	}
	if as.loading == nil {
		as.loading = make(chan struct{})
		go func() {
			a, err := llm.LoadAssistant()
			as.mu.Lock()
			as.a, as.loadErr, as.lastUse = a, err, time.Now()
			close(as.loading)
			as.loading = nil
			as.mu.Unlock()
			if err == nil {
				go as.unloadWhenIdle()
			}
		}()
	}
	ch := as.loading
	as.mu.Unlock()
	<-ch
	as.mu.Lock()
	defer as.mu.Unlock()
	if as.a == nil {
		return nil, as.loadErr
	}
	as.lastUse = time.Now()
	return as.a, nil
}

// unloadWhenIdle frees the model's memory once it hasn't been used for a
// while; the next project loads it again (about two seconds).
func (as *assistant) unloadWhenIdle() {
	for {
		time.Sleep(time.Minute)
		as.mu.Lock()
		if as.a == nil {
			as.mu.Unlock()
			return
		}
		if time.Since(as.lastUse) > assistantIdle {
			as.a = nil
			as.mu.Unlock()
			return
		}
		as.mu.Unlock()
	}
}

// Choice is one way to run the project (a command and its environment)
// and the model's score for it.
type Choice struct {
	project.Option
	Score float64 `json:"score"`
}

// rank orders a project's options, most likely first (see Plan.Ranked).
func (as *assistant) rank(p project.Plan) ([]Choice, error) {
	if len(p.Options) == 0 {
		return nil, errors.New("no commands to choose from")
	}
	a, err := as.get()
	if err != nil {
		return nil, err
	}
	short := make([]string, len(p.Options))
	for i, o := range p.Options {
		short[i] = o.ForModel()
	}
	scores, err := a.Rank(llm.Facts{Language: p.Facts.Language, Files: p.Facts.Files, Details: p.Facts.Details}, short)
	if err != nil {
		return nil, err
	}
	var out []Choice
	for _, i := range p.Ranked(scores) {
		out = append(out, Choice{Option: p.Options[i], Score: scores[i]})
	}
	return out, nil
}
