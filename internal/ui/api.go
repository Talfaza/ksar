package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"time"

	"ksar/internal/image"
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
		st.CacheDir, _ = image.CacheDir()
		st.ImagesDir = s.cfg.ImagesDir
		st.Addr = s.ln.Addr().String()
		writeJSON(w, st)
	})

	mux.HandleFunc("GET /api/runs", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, s.runs.list())
	})
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
	mux.HandleFunc("GET /api/runs/{id}/events", s.streamRun)

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
		writeJSON(w, map[string]int64{"freed": freed})
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
