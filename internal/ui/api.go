package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"time"

	"golang.org/x/net/websocket"
	"io"
	"ksar/internal/desktop"
	"ksar/internal/image"
	"ksar/internal/llm"
	"ksar/internal/project"
	"net"
)

// Status describes this machine for the sidebar and settings page.
type Status struct {
	KVM           bool   `json:"kvm"`
	VhostVsock    bool   `json:"vhost_vsock"`
	Kernel        string `json:"kernel"`         // path
	KernelVersion string `json:"kernel_version"` // e.g. 6.18.57-ksar
	KsarKernel    bool   `json:"ksar_kernel"`
	Cgroups       bool   `json:"cgroups"`
	MkfsErofs     bool   `json:"mkfs_erofs"`
	Docker        bool   `json:"docker"`
	Lockdown      bool   `json:"lockdown"`
	ImagesDir     string `json:"images_dir"`
	CacheDir      string `json:"cache_dir"`
	CacheBytes    int64  `json:"cache_bytes"`
	CacheImages   int    `json:"cache_images"`
	Addr          string `json:"addr"`
	PID           int    `json:"pid"`
}

const maxJSONBody = 64 << 10

func (s *Server) apiRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/status", func(w http.ResponseWriter, r *http.Request) {
		st := s.cfg.Status()
		st.CacheBytes, st.CacheImages = image.CacheUsage()
		_, layerBytes := desktop.Layers() // their disks are already in CacheUsage
		st.CacheBytes += layerBytes
		st.CacheDir, _ = image.CacheDir()
		st.ImagesDir = s.cfg.ImagesDir
		st.Addr = s.ln.Addr().String()
		writeJSON(w, st)
	})

	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.runs.list())
	})
	mux.HandleFunc("GET /api/runs/events", s.streamRuns)
	mux.HandleFunc("POST /api/runs", func(w http.ResponseWriter, r *http.Request) {
		var spec RunSpec
		if !readJSON(w, r, &spec) {
			return
		}
		run, err := s.runs.start(spec)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, run.snapshot())
	})
	mux.HandleFunc("GET /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if run, ok := s.runs.get(r.PathValue("id")); ok {
			writeJSON(w, run.snapshot())
			return
		}
		writeError(w, http.StatusNotFound, errors.New("no such run"))
	})
	mux.HandleFunc("POST /api/runs/{id}/stop", func(w http.ResponseWriter, r *http.Request) {
		run, ok := s.runs.get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("no such run"))
			return
		}
		run.stop()
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/runs/{id}/stdin", func(w http.ResponseWriter, r *http.Request) {
		run, ok := s.runs.get(r.PathValue("id"))
		if !ok {
			writeError(w, http.StatusNotFound, errors.New("no such run"))
			return
		}
		var body struct {
			Data  string `json:"data"`
			Close bool   `json:"close"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		if err := run.writeStdin(body.Data, body.Close); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("DELETE /api/runs/{id}", func(w http.ResponseWriter, r *http.Request) {
		if _, err := s.runs.forget(r.PathValue("id")); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	// Clear the history: every finished run (running ones stay).
	mux.HandleFunc("DELETE /api/runs", func(w http.ResponseWriter, r *http.Request) {
		n, _ := s.runs.forget("")
		writeJSON(w, map[string]int{"deleted": n})
	})
	mux.HandleFunc("GET /api/history", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.runs.historyInfo())
	})
	mux.HandleFunc("GET /api/runs/{id}/events", s.streamRun)
	mux.HandleFunc("GET /api/runs/{id}/vnc", s.vnc)

	mux.HandleFunc("POST /api/uploads", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+1<<20)
		f, hdr, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("upload: %w", err))
			return
		}
		defer f.Close()
		u, err := s.up.add(hdr.Filename, f)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, u)
	})

	mux.HandleFunc("POST /api/uploads/dir", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxDirUploadBytes+16<<20)
		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("upload: %w", err))
			return
		}
		u, err := s.up.addDir(r.URL.Query().Get("name"), mr)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, u)
	})

	// An archive (.zip, .tar, .tar.gz) is unpacked into a folder upload.
	mux.HandleFunc("POST /api/uploads/archive", func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, maxDirUploadBytes+1<<20)
		f, hdr, err := r.FormFile("file")
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("upload: %w", err))
			return
		}
		defer f.Close()
		if !IsArchive(hdr.Filename) {
			writeError(w, http.StatusBadRequest, errors.New("not a .zip, .tar or .tar.gz file"))
			return
		}
		u, err := s.up.addArchive(hdr.Filename, f)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, u)
	})

	// Suggests how to run an uploaded folder (see project.go).
	mux.HandleFunc("POST /api/projects/detect", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Upload string `json:"upload"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		u, ok := s.up.get(req.Upload)
		if !ok || !u.Dir {
			writeError(w, http.StatusNotFound, errors.New("no such uploaded folder"))
			return
		}
		writeJSON(w, s.analyze(u.path))
	})

	// The command assistant (assistant.go): status, one-time download, and
	// ranking a project's candidate commands.
	mux.HandleFunc("GET /api/assistant", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.ai.status())
	})
	mux.HandleFunc("POST /api/assistant/download", func(w http.ResponseWriter, r *http.Request) {
		s.ai.download()
		writeJSON(w, s.ai.status())
	})
	mux.HandleFunc("DELETE /api/assistant", func(w http.ResponseWriter, r *http.Request) {
		if err := s.ai.remove(); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, s.ai.status())
	})
	mux.HandleFunc("POST /api/projects/rank", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Upload string `json:"upload"`
		}
		if !readJSON(w, r, &req) {
			return
		}
		u, ok := s.up.get(req.Upload)
		if !ok || !u.Dir {
			writeError(w, http.StatusNotFound, errors.New("no such uploaded folder"))
			return
		}
		start := time.Now()
		choices, err := s.ai.rank(s.analyze(u.path))
		if err != nil {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		writeJSON(w, map[string]any{"choices": choices, "model": llm.ModelName, "ms": time.Since(start).Milliseconds()})
	})

	mux.HandleFunc("GET /api/images", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.images.list())
	})
	mux.HandleFunc("POST /api/images/import", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Ref string `json:"ref"`
		}
		if !readJSON(w, r, &body) {
			return
		}
		name, err := s.images.importDocker(body.Ref)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]string{"name": name})
	})
	// Install an image's desktop ahead of its first display run.
	mux.HandleFunc("POST /api/images/{name}/desktop", func(w http.ResponseWriter, r *http.Request) {
		if err := s.images.prepareDesktop(r.PathValue("name"), s.cfg); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("DELETE /api/images/{name}", func(w http.ResponseWriter, r *http.Request) {
		if err := s.images.remove(r.PathValue("name")); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/images/prune", func(w http.ResponseWriter, r *http.Request) {
		freed, err := s.images.prune()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, map[string]int64{"freed": freed})
	})
	mux.HandleFunc("POST /api/cache/clear", func(w http.ResponseWriter, r *http.Request) {
		freed, err := image.Prune(nil)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		more, err := desktop.Clear() // desktop layers are rebuilt on the next display run
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, map[string]int64{"freed": freed + more})
	})

	mux.HandleFunc("GET /api/settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.set.get())
	})
	mux.HandleFunc("PUT /api/settings", func(w http.ResponseWriter, r *http.Request) {
		var v Settings
		if !readJSON(w, r, &v) {
			return
		}
		if err := s.set.put(v); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, v)
	})
}

// vnc bridges a WebSocket to the run's VNC socket, so the page can show
// the guest's screen. The browser speaks RFB itself; this only moves
// bytes. A WebSocket upgrade is a GET, which the middleware lets through
// without an Origin check, so it is checked here: cookies aren't
// isolated by port, and a page on another local port must not be able to
// open the screen with the user's session.
func (s *Server) vnc(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runs.get(r.PathValue("id"))
	if !ok || run.vncSock == "" {
		writeError(w, http.StatusNotFound, errors.New("this run has no display"))
		return
	}
	if run.finished() {
		writeError(w, http.StatusGone, errors.New("the run has ended"))
		return
	}
	srv := websocket.Server{
		Handshake: func(c *websocket.Config, req *http.Request) error {
			if req.Header.Get("Origin") != s.origin {
				return errors.New("cross-origin request refused")
			}
			return nil
		},
		Handler: func(ws *websocket.Conn) {
			defer ws.Close()
			ws.PayloadType = websocket.BinaryFrame
			conn, err := net.DialTimeout("unix", run.vncSock, 5*time.Second)
			if err != nil {
				return
			}
			defer conn.Close()
			go func() {
				io.Copy(conn, ws)
				conn.Close()
			}()
			io.Copy(ws, conn)
		},
	}
	srv.ServeHTTP(w, r)
}

// streamRuns pushes the run list as Server-Sent Events: once on connect,
// then whenever it changes, at most a few times a second. Nothing is sent
// while no run changes, so an idle page costs nothing.
func (s *Server) streamRuns(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		changed := s.runs.changes() // before the snapshot, so no change is missed
		b, _ := json.Marshal(s.runs.list())
		if _, err := fmt.Fprintf(w, "event: runs\ndata: %s\n\n", b); err != nil {
			return
		}
		flusher.Flush()
	wait:
		for {
			select {
			case <-r.Context().Done():
				return
			case <-heartbeat.C:
				if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
					return
				}
				flusher.Flush()
			case <-changed:
				break wait
			}
		}
		select { // coalesce bursts of changes
		case <-r.Context().Done():
			return
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// streamRun sends a run's output as Server-Sent Events. Each output chunk
// carries its sequence number as the event id, so a reconnecting browser
// (Last-Event-ID) resumes exactly where it left off.
func (s *Server) streamRun(w http.ResponseWriter, r *http.Request) {
	run, ok := s.runs.get(r.PathValue("id"))
	if !ok {
		writeError(w, http.StatusNotFound, errors.New("no such run"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}
	after := -1
	if v, err := strconv.Atoi(r.Header.Get("Last-Event-ID")); err == nil {
		after = v
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")

	backlog, ch := run.subscribe(after)
	defer func() { run.unsubscribe(ch) }()
	last := after
	send := func(c Chunk) bool {
		var err error
		if c.Stream == "state" {
			b, _ := json.Marshal(run.snapshot())
			_, err = fmt.Fprintf(w, "event: state\ndata: %s\n\n", b)
		} else {
			b, _ := json.Marshal(c)
			_, err = fmt.Fprintf(w, "id: %d\nevent: chunk\ndata: %s\n\n", c.Seq, b)
			last = c.Seq
		}
		return err == nil
	}
	if !send(Chunk{Stream: "state"}) {
		return
	}
	for _, c := range backlog {
		if !send(c) {
			return
		}
	}
	flusher.Flush()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": keep-alive\n\n"); err != nil {
				return
			}
		case c, open := <-ch:
			if !open && !run.finished() {
				// This client fell behind: resync from the last chunk it got.
				var more []Chunk
				more, ch = run.subscribe(last)
				for _, c := range more {
					if !send(c) {
						return
					}
				}
				flusher.Flush()
				continue
			}
			if !open {
				// Run finished: send what's left, the final state, and
				// tell the browser not to reconnect.
				rest, _ := run.subscribe(last)
				for _, c := range rest {
					send(c)
				}
				send(Chunk{Stream: "state"})
				fmt.Fprint(w, "event: end\ndata: {}\n\n")
				flusher.Flush()
				return
			}
			if !send(c) {
				return
			}
		}
		flusher.Flush()
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, errors.New("expected application/json"))
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxJSONBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf("bad request body: %w", err))
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// analyze is project.Analyze with each option's image resolved to the
// library's copy, when it has been imported already.
func (s *Server) analyze(dir string) project.Plan {
	plan := project.Analyze(dir)
	have := func(ref string) string {
		if _, ok := s.images.path(project.LibraryName(ref)); ok {
			return project.LibraryName(ref)
		}
		return ""
	}
	plan.Image = have(plan.Docker)
	for i := range plan.Options {
		plan.Options[i].Image = have(plan.Options[i].Docker)
	}
	return plan
}
