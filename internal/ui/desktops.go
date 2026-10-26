package ui

import (
	"bytes"
	"errors"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"ksar/internal/desktop"
)

// Desktops prepared ahead of time: the first display run of an image
// installs XFCE into a cached layer, which takes minutes. Preparing it
// from the Images page does that in the background, in the same kind of
// install VM (a separate ksar run whose network reaches only the
// distribution's mirrors), so the first display run starts in seconds.

// Desktop states reported with each image.
const (
	desktopReady     = "ready"     // a display run starts in seconds
	desktopPreparing = "preparing" // being installed now
	desktopFailed    = "failed"    // the last attempt failed (DesktopNote says why)
	desktopNone      = ""          // first display run installs it
)

type desktopJob struct {
	state string
	note  string // last line of the install's output, or the error
}

// desktopState is an image's desktop state, without reading the image.
func (l *imageLibrary) desktopState(name, tarPath string) (string, string) {
	l.mu.Lock()
	job := l.desktops[name]
	l.mu.Unlock()
	if job != nil && job.state == desktopPreparing {
		return job.state, job.note
	}
	if _, ok := desktop.Cached(tarPath); ok {
		return desktopReady, ""
	}
	if info, ok := desktop.Inspected(tarPath); ok && info.Ready() {
		return desktopReady, "" // the image has a desktop of its own
	}
	if job != nil {
		return job.state, job.note
	}
	return desktopNone, ""
}

// prepareDesktop starts installing an image's desktop in the background.
func (l *imageLibrary) prepareDesktop(name string, cfg Config) error {
	p, ok := l.path(name)
	if !ok {
		return errors.New("no such image")
	}
	l.mu.Lock()
	if j := l.desktops[name]; j != nil && j.state == desktopPreparing {
		l.mu.Unlock()
		return nil
	}
	job := &desktopJob{state: desktopPreparing, note: "Starting the install VM…"}
	l.desktops[name] = job
	l.mu.Unlock()

	set := func(state, note string) {
		l.mu.Lock()
		job.state, job.note = state, note
		l.mu.Unlock()
	}
	go func() {
		info, err := desktop.Inspect(p)
		if err == nil && info.Ready() {
			set(desktopReady, "")
			return
		}
		if err == nil {
			_, err = desktop.RecipeFor(info)
		}
		if err == nil {
			_, err = desktop.Ensure(p, uiBuilder(cfg, func(line string) { set(desktopPreparing, line) }), &bytes.Buffer{})
		}
		if err != nil {
			set(desktopFailed, err.Error())
			return
		}
		set(desktopReady, "")
	}()
	return nil
}

// uiBuilder runs the install as a ksar run child, reporting each output
// line (guest output is filtered by the child: its stderr isn't a
// terminal, so it applies the same filter as for the browser).
func uiBuilder(cfg Config, progress func(string)) desktop.Builder {
	return func(args []string, _ io.Writer) error {
		argv := []string{"run"}
		if cfg.Kernel != "" {
			argv = append(argv, "-kernel", cfg.Kernel)
		}
		cmd := exec.Command(cfg.KsarPath, append(argv, args...)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
		var tail lineTail
		tail.onLine = progress
		cmd.Stdout, cmd.Stderr = &tail, &tail
		if err := cmd.Run(); err != nil {
			if last := tail.last(); last != "" {
				return errors.New(last)
			}
			return err
		}
		return nil
	}
}

// lineTail reports complete output lines and remembers the last ones.
type lineTail struct {
	mu     sync.Mutex
	buf    []byte
	lines  []string
	onLine func(string)
}

func (t *lineTail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	for {
		i := bytes.IndexAny(t.buf, "\r\n")
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(t.buf[:i]))
		t.buf = t.buf[i+1:]
		if line == "" || strings.Contains(line, "net: blocked lookup _http._tcp") {
			continue
		}
		if len(line) > 160 {
			line = line[:160] + "…"
		}
		t.lines = append(t.lines, line)
		if len(t.lines) > 20 {
			t.lines = t.lines[1:]
		}
		if t.onLine != nil {
			t.onLine(line)
		}
	}
	if len(t.buf) > 64<<10 {
		t.buf = t.buf[len(t.buf)-4096:]
	}
	return len(p), nil
}

func (t *lineTail) last() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.lines) == 0 {
		return ""
	}
	return t.lines[len(t.lines)-1]
}
