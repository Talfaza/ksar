// Package ui serves Ksar's local web interface.
//
// The server never runs guest code itself: every run is a separate
// `ksar run` process (with its own seccomp lockdown and cgroup scope),
// supervised from here. The server streams that process's output to the
// browser.
//
// Security model. The server listens on 127.0.0.1 only, but any web page
// the user visits, and any other local process, can still send requests
// to it, so:
//
//   - Host must be exactly our 127.0.0.1:PORT (or localhost:PORT), which
//     defeats DNS rebinding.
//   - Every request needs the session cookie, set by visiting the
//     one-time /auth?token=... link printed at startup. The cookie is
//     HttpOnly and SameSite=Strict.
//   - Every state-changing request must carry our exact Origin. Cookies
//     are shared across ports, so a page served by another local server
//     is "same-site" and would get the cookie; the Origin check stops it.
//   - Responses forbid framing and inline scripts (CSP), and the browser
//     only ever inserts guest output as text, never as HTML.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

//go:embed web
var webFiles embed.FS

// Config is what the server needs to know about this machine.
type Config struct {
	Addr      string // listen address; must be a loopback IP
	KsarPath  string // ksar binary used for runs
	Kernel    string // kernel path passed to runs ("" = ksar's default)
	ImagesDir string // directory of root filesystem tars
	// HistoryDir keeps finished runs across restarts ("" = memory only).
	HistoryDir string
	Status     func() Status
}

// Server is the web UI.
type Server struct {
	cfg    Config
	token  string
	host   []string // accepted Host header values
	origin string
	cookie string
	ln     net.Listener
	runs   *runManager
	images *imageLibrary
	up     *uploadStore
	set    *settingsStore
	ai     *assistant
	http   *http.Server
}

// New binds the listener and prepares the server. Call Serve to start.
func New(cfg Config) (*Server, error) {
	host, _, err := net.SplitHostPort(cfg.Addr)
	if err != nil {
		return nil, fmt.Errorf("listen address %q: %w", cfg.Addr, err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("refusing to listen on %q: the UI may only listen on a loopback address such as 127.0.0.1", cfg.Addr)
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		ln.Close()
		return nil, err
	}
	up, err := newUploadStore()
	if err != nil {
		ln.Close()
		return nil, err
	}
	s := &Server{
		cfg:    cfg,
		token:  hex.EncodeToString(tok),
		host:   []string{fmt.Sprintf("%s:%d", host, port), fmt.Sprintf("localhost:%d", port)},
		origin: fmt.Sprintf("http://%s:%d", host, port),
		cookie: "ksar_session_" + strconv.Itoa(port),
		ln:     ln,
		up:     up,
		set:    newSettingsStore(),
		images: newImageLibrary(cfg.ImagesDir),
		ai:     &assistant{},
	}
	s.runs = newRunManager(cfg, s.up, s.images)
	s.http = &http.Server{
		Handler:           s.secure(s.routes()),
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}
	return s, nil
}

// URL is the one-time login link to open in the browser.
func (s *Server) URL() string { return s.origin + "/auth?token=" + s.token }

// Origin is the server's base URL.
func (s *Server) Origin() string { return s.origin }

// Serve handles requests until ctx is cancelled, then stops all runs.
func (s *Server) Serve(ctx context.Context) error {
	errc := make(chan error, 1)
	go func() { errc <- s.http.Serve(s.ln) }()
	select {
	case <-ctx.Done():
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			s.runs.stopAll()
			s.up.close()
			return err
		}
	}
	s.runs.stopAll()
	shutdown, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	s.http.Shutdown(shutdown)
	s.up.close()
	return nil
}

const csp = "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self'; " +
	"connect-src 'self'; font-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'"

// secure wraps every handler with the checks described in the package doc.
func (s *Server) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")

		if !s.validHost(r.Host) {
			http.Error(w, "unexpected Host header", http.StatusMisdirectedRequest)
			return
		}
		if r.URL.Path == "/auth" {
			s.login(w, r)
			return
		}
		if r.Method == http.MethodGet && publicAssets[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		if !s.authenticated(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				http.Error(w, "not signed in", http.StatusUnauthorized)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, signInPage)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if r.Header.Get("Origin") != s.origin {
				http.Error(w, "cross-origin request refused", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) validHost(h string) bool {
	for _, v := range s.host {
		if h == v {
			return true
		}
	}
	return false
}

func (s *Server) authenticated(r *http.Request) bool {
	c, err := r.Cookie(s.cookie)
	return err == nil && subtle.ConstantTimeCompare([]byte(c.Value), []byte(s.token)) == 1
}

// login exchanges the startup token for a session cookie, then redirects
// to a clean URL so the token doesn't stay in the address bar or history.
func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet ||
		subtle.ConstantTimeCompare([]byte(r.URL.Query().Get("token")), []byte(s.token)) != 1 {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, signInPage)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookie, Value: s.token, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode,
	})
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	static, _ := fs.Sub(webFiles, "web")
	files := http.FileServerFS(static)
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	}))
	s.apiRoutes(mux)
	return mux
}

// publicAssets are served without a session: the sign-in page needs
// them, and they contain nothing sensitive.
var publicAssets = map[string]bool{"/app.css": true, "/mascot.png": true, "/mascot-missing.png": true}

const signInPage = `<!doctype html><html lang="en"><head><meta charset="utf-8">
<title>Ksar: sign in</title><link rel="stylesheet" href="/app.css"></head>
<body class="signin"><main><img src="/mascot-missing.png" alt="" width="96" height="96">
<h1>Open Ksar from your terminal</h1>
<p>For your safety, this page only works through the one-time link that
<code>ksar ui</code> printed when it started. Open that link to sign in.</p>
</main></body></html>`
