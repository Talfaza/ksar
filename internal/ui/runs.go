package ui

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"ksar/internal/netproxy"
	"ksar/internal/termsafe"
	"ksar/internal/vmm"
)

// Limits on what a run request may ask for.
const (
	maxCommandLen = 4096
	maxEnv        = 64
	maxFiles      = 32
	minMemMB      = 64
	maxMemMB      = 3072
	maxTimeoutS   = 24 * 3600
	maxRuns       = 200     // runs kept in history
	maxLogBytes   = 4 << 20 // output kept per run, per stream group
)

// RunSpec is what the browser asks to run.
type RunSpec struct {
	Command     string     `json:"command"`
	Image       string     `json:"image"` // library image name, or "" for agent only
	MemMB       int        `json:"mem_mb"`
	TimeoutS    int        `json:"timeout_s"`
	Env         []string   `json:"env"`
	Workdir     string     `json:"workdir"`
	Files       []FileSpec `json:"files"`
	Interactive bool       `json:"interactive"`
	Display     string     `json:"display"`         // "" (none) or WIDTHxHEIGHT
	Net         string     `json:"net"`             // none (or ""), allow, internet
	Allow       []string   `json:"allow"`           // hosts for Net "allow" (and openings for "internet")
	Label       string     `json:"label,omitempty"` // what the page calls the run instead of the command (display only)
}

const maxAllow = 64

// validDisplay checks a screen size the way ksar run will.
func validDisplay(s string) error {
	if s == "" {
		return nil
	}
	var w, h int
	if n, err := fmt.Sscanf(s, "%dx%d", &w, &h); n != 2 || err != nil || fmt.Sprintf("%dx%d", w, h) != s {
		return fmt.Errorf("display %q: want WIDTHxHEIGHT", s)
	}
	if w < 320 || h < 200 || w > vmm.MaxDisplayWidth || h > vmm.MaxDisplayHeight || w%8 != 0 {
		return fmt.Errorf("display %q: must be between 320x200 and %dx%d", s, vmm.MaxDisplayWidth, vmm.MaxDisplayHeight)
	}
	return nil
}

// validNet checks a network policy the way ksar run will.
func validNet(mode string, allow []string) error {
	if len(allow) > maxAllow {
		return fmt.Errorf("at most %d allowed hosts", maxAllow)
	}
	switch mode {
	case "", netproxy.ModeNone:
		return nil // the allow list is kept, but unused
	case netproxy.ModeAllow, netproxy.ModeInternet:
	default:
		return fmt.Errorf("network must be none, allow or internet, not %q", mode)
	}
	for _, a := range allow {
		if _, err := netproxy.ParseRule(a); err != nil {
			return err
		}
	}
	if mode == netproxy.ModeAllow && len(allow) == 0 {
		return errors.New("add at least one allowed host, or turn the network off")
	}
	return nil
}

// FileSpec copies an uploaded file into the guest.
type FileSpec struct {
	Upload string `json:"upload"`
	Name   string `json:"name"`
	Path   string `json:"path"`
}

func (s *RunSpec) validate(lib *imageLibrary, up *uploadStore) ([]string, error) {
	if len(s.Command) > maxCommandLen || strings.ContainsRune(s.Command, 0) {
		return nil, errors.New("command is too long or contains a NUL byte")
	}
	if len(s.Label) > 300 || strings.ContainsAny(s.Label, "\x00\n\r") {
		return nil, errors.New("label is too long or has control characters")
	}
	var args []string
	if strings.TrimSpace(s.Command) == "" {
		if s.Display == "" {
			return nil, errors.New("command is empty")
		}
		// A display run with no command is the desktop itself.
	} else {
		var err error
		if args, err = splitCommand(s.Command); err != nil {
			return nil, err
		}
	}
	if s.MemMB < minMemMB || s.MemMB > maxMemMB {
		return nil, fmt.Errorf("memory must be between %d and %d MB", minMemMB, maxMemMB)
	}
	if s.TimeoutS < 1 || s.TimeoutS > maxTimeoutS {
		return nil, fmt.Errorf("timeout must be between 1 and %d seconds", maxTimeoutS)
	}
	if len(s.Env) > maxEnv {
		return nil, fmt.Errorf("at most %d environment variables", maxEnv)
	}
	for _, e := range s.Env {
		k, _, ok := strings.Cut(e, "=")
		if !ok || k == "" || strings.ContainsRune(e, 0) {
			return nil, fmt.Errorf("environment entry %q: want KEY=VALUE", e)
		}
	}
	if s.Workdir != "" && !path.IsAbs(s.Workdir) {
		return nil, errors.New("working directory must be an absolute path")
	}
	if len(s.Files) > maxFiles {
		return nil, fmt.Errorf("at most %d files", maxFiles)
	}
	for _, f := range s.Files {
		if !path.IsAbs(f.Path) || strings.ContainsRune(f.Path, 0) {
			return nil, fmt.Errorf("guest path %q must be absolute", f.Path)
		}
		if _, ok := up.get(f.Upload); !ok {
			return nil, fmt.Errorf("file %q is no longer uploaded", f.Name)
		}
	}
	if s.Image == "" && len(args) == 3 && args[0] == "sh" && args[2] == s.Command {
		return nil, errors.New("this command uses shell syntax (; && | > $ * …), which needs a shell: " +
			"choose an image (the agent-only guest has no /bin/sh)")
	}
	if err := validNet(s.Net, s.Allow); err != nil {
		return nil, err
	}
	if err := validDisplay(s.Display); err != nil {
		return nil, err
	}
	if s.Display != "" && s.Image == "" {
		return nil, errors.New("a display needs an image with a desktop (Xorg); the agent-only guest can't draw anything")
	}
	if s.Image != "" {
		if _, ok := lib.path(s.Image); !ok {
			return nil, fmt.Errorf("image %q not found", s.Image)
		}
	}
	return args, nil
}

// Chunk is one piece of a run's output.
type Chunk struct {
	Seq    int    `json:"seq"`
	Stream string `json:"stream"` // stdout, stderr, console, ksar
	Data   string `json:"data"`
}

// RunInfo is a run's state as sent to the browser.
type RunInfo struct {
	ID          string    `json:"id"`
	Spec        RunSpec   `json:"spec"`
	Args        []string  `json:"args"`
	Status      string    `json:"status"` // preparing, booting, running, succeeded, failed, timed_out, stopped, error
	ExitCode    *int      `json:"exit_code,omitempty"`
	Error       string    `json:"error,omitempty"`
	Created     time.Time `json:"created"`
	Ended       time.Time `json:"ended,omitzero"`
	BootMs      int64     `json:"boot_ms,omitempty"`
	CID         uint32    `json:"cid,omitempty"`
	Scope       string    `json:"scope,omitempty"`
	PID         int       `json:"pid,omitempty"`
	Virtio      bool      `json:"virtio"`
	Kernel      string    `json:"kernel,omitempty"`
	Lockdown    bool      `json:"lockdown"`
	MemLimit    int64     `json:"mem_limit,omitempty"`
	MemCurrent  int64     `json:"mem_current,omitempty"`
	MemPeak     int64     `json:"mem_peak,omitempty"`
	CPUPercent  float64   `json:"cpu_percent"`
	Filtered    int       `json:"filtered"` // control sequences removed from output
	Truncated   bool      `json:"truncated"`
	Interactive bool      `json:"interactive"`
	Display     *Screen   `json:"display,omitempty"`
	Net         string    `json:"net"`          // effective mode
	NetConns    int       `json:"net_conns"`    // connections opened
	NetBlocked  int       `json:"net_blocked"`  // lookups and connections refused
	NetSent     int64     `json:"net_sent"`     // bytes, guest to world
	NetReceived int64     `json:"net_received"` // bytes, world to guest
}

// Screen is a run's display size.
type Screen struct {
	Width  int `json:"width"`
	Height int `json:"height"`
}

type run struct {
	mu   sync.Mutex
	info RunInfo
	log  []Chunk
	size map[string]int // bytes kept per stream
	subs map[chan Chunk]struct{}
	done chan struct{}

	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stopped  bool
	files    []string // upload ids to delete when the run ends
	filters  map[string]*termsafe.Writer
	onChange func() // tells the run list its state changed
	vncSock  string // the run's VNC Unix socket, if it has a display
}

type runManager struct {
	cfg  Config
	up   *uploadStore
	lib  *imageLibrary
	mu   sync.Mutex
	runs map[string]*run
	seq  int

	changeMu sync.Mutex
	changed  chan struct{} // closed (and replaced) whenever any run changes

	sockOnce sync.Once
	sockDir  string // private directory (0700) for runs' VNC sockets
	sockErr  error
}

// socketDir returns the private directory for VNC sockets: only this
// user can reach a run's screen, and only through the UI.
func (m *runManager) socketDir() (string, error) {
	m.sockOnce.Do(func() {
		base := os.Getenv("XDG_RUNTIME_DIR")
		if base == "" {
			base = os.TempDir()
		}
		m.sockDir, m.sockErr = os.MkdirTemp(base, "ksar-ui-")
	})
	return m.sockDir, m.sockErr
}

func newRunManager(cfg Config, up *uploadStore, lib *imageLibrary) *runManager {
	m := &runManager{cfg: cfg, up: up, lib: lib, runs: map[string]*run{}, changed: make(chan struct{})}
	m.loadHistory()
	return m
}

// changes returns a channel that is closed at the next change to the run
// list (a run starting, changing state, or its metrics updating).
func (m *runManager) changes() <-chan struct{} {
	m.changeMu.Lock()
	defer m.changeMu.Unlock()
	return m.changed
}

func (m *runManager) bump() {
	m.changeMu.Lock()
	close(m.changed)
	m.changed = make(chan struct{})
	m.changeMu.Unlock()
}

func (m *runManager) list() []RunInfo {
	m.mu.Lock()
	runs := make([]*run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	out := make([]RunInfo, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.snapshot())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.After(out[j].Created) })
	return out
}

func (m *runManager) get(id string) (*run, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	return r, ok
}

// start launches `ksar run` for spec as a child process.
func (m *runManager) start(spec RunSpec) (*run, error) {
	args, err := spec.validate(m.lib, m.up)
	if err != nil {
		return nil, err
	}
	argv := []string{"run", "-events-fd", "3", "-console-fd", "4",
		"-mem", strconv.Itoa(spec.MemMB) + "M", "-timeout", strconv.Itoa(spec.TimeoutS) + "s"}
	if m.cfg.Kernel != "" {
		argv = append(argv, "-kernel", m.cfg.Kernel)
	}
	if spec.Image != "" {
		p, _ := m.lib.path(spec.Image)
		argv = append(argv, "-image", p)
	}
	if spec.Workdir != "" {
		argv = append(argv, "-w", spec.Workdir)
	}
	for _, e := range spec.Env {
		argv = append(argv, "-e", e)
	}
	var uploads []string
	for _, f := range spec.Files {
		u, _ := m.up.get(f.Upload)
		argv = append(argv, "-copy", u.path+":"+f.Path)
		uploads = append(uploads, f.Upload)
	}
	if spec.Interactive {
		argv = append(argv, "-i")
	}
	id := newID()
	argv = append(argv, "-id", id) // the same id in ksar ps and ksar stop
	var vncSock string
	if spec.Display != "" {
		dir, err := m.socketDir()
		if err != nil {
			return nil, fmt.Errorf("display socket: %w", err)
		}
		vncSock = filepath.Join(dir, id+".sock")
		// -desktop: start X and XFCE, installing them into a cached layer
		// first if the image has none; the command runs on the desktop.
		argv = append(argv, "-display", spec.Display, "-vnc", "unix:"+vncSock, "-desktop")
	}
	if spec.Net != "" && spec.Net != netproxy.ModeNone {
		argv = append(argv, "-net", spec.Net)
		for _, a := range spec.Allow {
			argv = append(argv, "-allow", a)
		}
	}
	argv = append(argv, "--")
	argv = append(argv, args...)

	evR, evW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	conR, conW, err := os.Pipe()
	if err != nil {
		evR.Close()
		evW.Close()
		return nil, err
	}
	cmd := exec.Command(m.cfg.KsarPath, argv...)
	cmd.ExtraFiles = []*os.File{evW, conW}        // fds 3 and 4
	cmd.Env = append(os.Environ(), "KSAR_SCOPE=") // never inherit a scope marker
	// If the UI dies, its runs die with it rather than linger unsupervised.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	var stdin io.WriteCloser
	if spec.Interactive {
		stdin, _ = cmd.StdinPipe()
	}

	r := &run{
		info: RunInfo{ID: id, Spec: spec, Args: args, Status: "booting", Created: time.Now(),
			Interactive: spec.Interactive, Net: netproxy.ModeNone},
		size: map[string]int{}, subs: map[chan Chunk]struct{}{}, done: make(chan struct{}),
		cmd: cmd, stdin: stdin, files: uploads,
		filters: map[string]*termsafe.Writer{}, onChange: m.bump, vncSock: vncSock,
	}
	if err := cmd.Start(); err != nil {
		evR.Close()
		evW.Close()
		conR.Close()
		conW.Close()
		return nil, fmt.Errorf("start ksar run: %w", err)
	}
	evW.Close()
	conW.Close()
	r.info.PID = cmd.Process.Pid

	m.mu.Lock()
	m.runs[id] = r
	dropped := m.trimLocked()
	m.mu.Unlock()
	m.bump()
	m.save(r, false)
	if m.cfg.HistoryDir != "" {
		for _, old := range dropped {
			os.Remove(filepath.Join(m.cfg.HistoryDir, old+".json.gz"))
		}
	}

	var wg sync.WaitGroup
	wg.Add(4)
	go func() { defer wg.Done(); r.pump("stdout", stdout) }()
	go func() { defer wg.Done(); r.pump("stderr", stderr) }()
	go func() { defer wg.Done(); r.pump("console", conR) }()
	go func() { defer wg.Done(); r.readEvents(evR) }()
	go r.watchMetrics()
	go func() {
		wg.Wait()
		err := cmd.Wait()
		r.finish(err)
		m.save(r, true)
		if vncSock != "" {
			os.Remove(vncSock) // ksar run can't: it is locked down
		}
		for _, u := range r.files {
			m.up.remove(u)
		}
	}()
	return r, nil
}

// trimLocked drops the oldest finished runs beyond maxRuns, returning
// their ids.
func (m *runManager) trimLocked() []string {
	if len(m.runs) <= maxRuns {
		return nil
	}
	var finished []*run
	for _, r := range m.runs {
		select {
		case <-r.done:
			finished = append(finished, r)
		default:
		}
	}
	sort.Slice(finished, func(i, j int) bool { return finished[i].info.Created.Before(finished[j].info.Created) })
	var dropped []string
	for _, r := range finished {
		if len(m.runs) <= maxRuns {
			break
		}
		delete(m.runs, r.info.ID)
		dropped = append(dropped, r.info.ID)
	}
	return dropped
}

func (m *runManager) stopAll() {
	m.mu.Lock()
	runs := make([]*run, 0, len(m.runs))
	for _, r := range m.runs {
		runs = append(runs, r)
	}
	m.mu.Unlock()
	for _, r := range runs {
		r.stop()
	}
	for _, r := range runs {
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
		}
	}
	if m.sockDir != "" {
		os.RemoveAll(m.sockDir)
	}
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// pump copies one output stream into the log. Guest output is filtered
// like terminal output (colors stay, other control sequences go), since
// the browser shows it as a terminal.
func (r *run) pump(stream string, rd io.ReadCloser) {
	defer rd.Close()
	f := termsafe.NewWriter(writerFunc(func(p []byte) (int, error) {
		r.append(stream, string(p))
		return len(p), nil
	}))
	r.mu.Lock()
	r.filters[stream] = f
	r.mu.Unlock()
	// Coalesce: output often arrives a byte at a time (the guest console is
	// a serial port), so gather what comes in over a short window into
	// one chunk instead of flooding the log and the browser.
	reads := make(chan []byte, 64)
	go func() {
		defer close(reads)
		for {
			buf := make([]byte, 32<<10)
			n, err := rd.Read(buf)
			if n > 0 {
				reads <- buf[:n]
			}
			if err != nil {
				return
			}
		}
	}()
	const window = 15 * time.Millisecond
	var pending []byte
	var flushAt <-chan time.Time
	for {
		select {
		case b, ok := <-reads:
			if !ok {
				f.Write(pending)
				f.Flush()
				return
			}
			if len(pending) == 0 {
				flushAt = time.After(window)
			}
			pending = append(pending, b...)
			if len(pending) >= 64<<10 {
				f.Write(pending)
				pending, flushAt = nil, nil
			}
		case <-flushAt:
			f.Write(pending)
			pending, flushAt = nil, nil
		}
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }

func (r *run) append(stream, data string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	group := stream
	if stream == "stdout" || stream == "stderr" {
		group = "output"
	}
	if r.size[group]+len(data) > maxLogBytes {
		r.info.Truncated = true
		return
	}
	r.size[group] += len(data)
	c := Chunk{Seq: len(r.log), Stream: stream, Data: data}
	r.log = append(r.log, c)
	for ch := range r.subs {
		select {
		case ch <- c:
		default: // slow subscriber: it will resync from its last seq
			close(ch)
			delete(r.subs, ch)
		}
	}
}

// readEvents applies the child's JSON events to the run's state.
func (r *run) readEvents(rd io.ReadCloser) {
	defer rd.Close()
	sc := bufio.NewScanner(rd)
	sc.Buffer(make([]byte, 64<<10), 64<<10)
	for sc.Scan() {
		var ev struct {
			Event    string          `json:"event"`
			Scope    string          `json:"scope"`
			Virtio   bool            `json:"virtio"`
			Kernel   string          `json:"kernel"`
			Lockdown bool            `json:"lockdown"`
			MemLimit int64           `json:"mem_limit"`
			BootMs   int64           `json:"boot_ms"`
			CID      uint32          `json:"cid"`
			Message  string          `json:"message"`
			Net      json.RawMessage `json:"net"`
		}
		if json.Unmarshal(sc.Bytes(), &ev) != nil {
			continue
		}
		r.mu.Lock()
		switch ev.Event {
		case "preparing":
			r.info.Status = "preparing"
		case "display":
			var d Screen
			if json.Unmarshal(sc.Bytes(), &d) == nil && d.Width > 0 {
				r.info.Display = &d
			}
		case "started":
			if r.info.Status == "preparing" {
				r.info.Status = "booting" // the desktop is installed; the run itself starts
			}
			r.info.Scope, r.info.Virtio, r.info.Kernel = ev.Scope, ev.Virtio, ev.Kernel
			r.info.Lockdown, r.info.MemLimit = ev.Lockdown, ev.MemLimit
			var n struct{ Mode string }
			if json.Unmarshal(ev.Net, &n) == nil && n.Mode != "" {
				r.info.Net = n.Mode
			}
		case "net":
			// Kept in the log as its own stream (JSON per chunk), so the
			// browser's network panel replays and resumes like output.
			var n netproxy.Event
			if json.Unmarshal(ev.Net, &n) != nil {
				break
			}
			switch {
			case n.Kind == "close":
				r.info.NetSent += n.Sent
				r.info.NetReceived += n.Received
			case !n.Allowed:
				r.info.NetBlocked++
			case n.Kind == "connect":
				r.info.NetConns++
			}
			b, _ := json.Marshal(n)
			r.mu.Unlock()
			r.append("net", string(b))
			r.mu.Lock()
		case "ready":
			r.info.Status, r.info.BootMs, r.info.CID = "running", ev.BootMs, ev.CID
		case "error":
			r.info.Error = ev.Message
		}
		r.mu.Unlock()
		r.notify()
	}
}

// notify tells live subscribers the run's state changed. State changes
// aren't stored in the log: a (re)connecting client fetches the current
// state anyway.
func (r *run) notify() {
	if r.onChange != nil {
		r.onChange()
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.subs {
		select {
		case ch <- Chunk{Seq: -1, Stream: "state"}:
		default: // already has a pending update or is behind
		}
	}
}

func (r *run) finish(waitErr error) {
	r.mu.Lock()
	code := 0
	var ee *exec.ExitError
	if errors.As(waitErr, &ee) {
		code = ee.ExitCode()
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			code = 128 + int(ws.Signal())
		}
	} else if waitErr != nil {
		code = 125
		r.info.Error = waitErr.Error()
	}
	r.info.ExitCode = &code
	r.info.Ended = time.Now()
	switch {
	case r.stopped, code == 128+int(syscall.SIGTERM): // here, or by ksar stop
		r.info.Status = "stopped"
	case code == 0:
		r.info.Status = "succeeded"
	case code == 124:
		r.info.Status = "timed_out"
	case code == 125:
		r.info.Status = "error"
	default:
		r.info.Status = "failed"
	}
	for _, f := range r.filters {
		r.info.Filtered += f.Removed()
	}
	r.mu.Unlock()
	r.notify()
	close(r.done)
	r.mu.Lock()
	for ch := range r.subs {
		close(ch)
		delete(r.subs, ch)
	}
	r.mu.Unlock()
}

// stop kills the run's ksar process; the VM lives inside it and dies too.
func (r *run) stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	select {
	case <-r.done:
		return
	default:
	}
	r.stopped = true
	if r.cmd.Process != nil {
		r.cmd.Process.Kill()
	}
}

func (r *run) writeStdin(data string, closeIt bool) error {
	r.mu.Lock()
	stdin := r.stdin
	r.mu.Unlock()
	if stdin == nil {
		return errors.New("this run was not started with interactive stdin")
	}
	if data != "" {
		if _, err := io.WriteString(stdin, data); err != nil {
			return err
		}
	}
	if closeIt {
		r.mu.Lock()
		r.stdin = nil
		r.mu.Unlock()
		return stdin.Close()
	}
	return nil
}

func (r *run) snapshot() RunInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	info := r.info
	if info.Status == "booting" || info.Status == "running" {
		filtered := 0
		for _, f := range r.filters {
			filtered += f.Removed()
		}
		info.Filtered = filtered
	}
	return info
}

// subscribe returns the chunks after seq and a channel for new ones. The
// channel is closed when the run ends or the subscriber falls behind; the
// caller tells the two apart with finished() and resubscribes if behind.
func (r *run) subscribe(after int) ([]Chunk, chan Chunk) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var backlog []Chunk
	if after+1 < len(r.log) {
		backlog = append(backlog, r.log[after+1:]...)
	}
	ch := make(chan Chunk, 512)
	select {
	case <-r.done:
		close(ch)
	default:
		r.subs[ch] = struct{}{}
	}
	return backlog, ch
}

// finished reports whether the run has ended.
func (r *run) finished() bool {
	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func (r *run) unsubscribe(ch chan Chunk) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.subs[ch]; ok {
		delete(r.subs, ch)
		close(ch)
	}
}

// watchMetrics samples the run's cgroup (memory, CPU) once a second.
func (r *run) watchMetrics() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	var lastUsec int64
	var lastAt time.Time
	for {
		select {
		case <-r.done:
			return
		case <-t.C:
		}
		r.mu.Lock()
		pid, scope := r.info.PID, r.info.Scope
		r.mu.Unlock()
		if scope == "" {
			continue // no cgroup of its own (cgroups unavailable or not yet entered)
		}
		dir, ok := cgroupDir(pid, scope)
		if !ok {
			continue
		}
		cur := readInt(dir + "/memory.current")
		peak := readInt(dir + "/memory.peak")
		usec := cpuUsec(dir)
		now := time.Now()
		r.mu.Lock()
		r.info.MemCurrent, r.info.MemPeak = cur, max(peak, r.info.MemPeak)
		if !lastAt.IsZero() && usec >= lastUsec {
			r.info.CPUPercent = float64(usec-lastUsec) / float64(now.Sub(lastAt).Microseconds()) * 100
		}
		r.mu.Unlock()
		lastUsec, lastAt = usec, now
		r.notify()
	}
}

// cgroupDir finds pid's cgroup directory, confirming it is the run's scope.
func cgroupDir(pid int, scope string) (string, bool) {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	if err != nil {
		return "", false
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if p, ok := strings.CutPrefix(line, "0::"); ok && strings.HasSuffix(p, "/"+scope) {
			return "/sys/fs/cgroup" + p, true
		}
	}
	return "", false
}

func readInt(p string) int64 {
	b, err := os.ReadFile(p)
	if err != nil {
		return 0
	}
	v, _ := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	return v
}

func cpuUsec(dir string) int64 {
	b, err := os.ReadFile(dir + "/cpu.stat")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "usage_usec "); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	return 0
}

// splitCommand splits a command line like a POSIX shell would for words
// and quotes ('...', "...", backslash), without any expansion. To get
// shell features, run a shell: sh -c '...'.
// shellChars are what make a command line need a shell when unquoted:
// ; & | (operators), < > (redirection), $ ` (expansion), * ? [ (globs),
// ( ) (subshells).
const shellChars = ";&|<>$`*?[()"

func splitCommand(s string) ([]string, error) {
	var args []string
	var cur strings.Builder
	inWord := false
	shell := false // unquoted shell syntax seen
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			end := strings.IndexByte(s[i+1:], '\'')
			if end < 0 {
				return nil, errors.New("unterminated ' quote")
			}
			cur.WriteString(s[i+1 : i+1+end])
			i += end + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && strings.IndexByte("\"\\$`\n", s[i+1]) >= 0 {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New(`unterminated " quote`)
			}
			inWord = true
		case c == '\\':
			if i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				inWord = true
			}
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				args = append(args, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			if strings.IndexByte(shellChars, c) >= 0 || c == '~' && !inWord {
				shell = true
			}
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		args = append(args, cur.String())
	}
	if len(args) == 0 {
		return nil, errors.New("command is empty")
	}
	if shell {
		// Operators, redirections, variables or globs: the user means a
		// shell command line, so the guest's shell runs it as typed.
		return []string{"sh", "-c", s}, nil
	}
	return args, nil
}
