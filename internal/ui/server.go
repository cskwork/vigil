// Package ui serves a single-page live view of what vigil and its Browser
// Agent are doing (PRD §15 evidence, §18 corpus health) for non-developers.
//
// It reads SQLite + evidence files and exposes two operator controls.
// POST/DELETE /api/schedule/window updates active hours. POST /api/requests
// delegates a bounded read-only QA request to the loop process when configured.
// Both controls are unauthenticated, so bind `serve`/`loop --ui` to a trusted
// address.
//
// File layout: server.go (routes, assets, path safety), todo.go (/api/todo,
// the action queue behind the landing page), overview.go
// (/api/overview), verification.go (/api/verification), evidence.go (run
// steps/detail and the per-run evidence summary cache), agent.go (Browser
// Agent transcript view), requests_list.go (GET /api/requests),
// request_submit.go (POST /api/requests), scripts.go (/api/scripts,
// /api/script), script_actions.go (POST /api/script/{run,approve,reject}), window.go (/api/schedule/window), supervisor.go
// (/api/supervisor), format.go (shared JSON/format helpers).
package ui

import (
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"vigil/internal/config"
	"vigil/internal/store"
)

//go:embed todo.html
var todoHTML []byte

//go:embed results.html
var resultsHTML []byte

//go:embed help.html
var helpHTML []byte

//go:embed activity.html
var activityHTML []byte

//go:embed report.html
var reportHTML []byte

//go:embed scripts.html
var scriptsHTML []byte

//go:embed theme.css
var themeCSS []byte

//go:embed app.js
var appJS []byte

type Server struct {
	cfg              *config.Config
	st               *store.Store
	evRoot           string
	devDir           string // when set, page assets are read from this directory on every request
	requestSubmitter RequestSubmitter
	scriptActions    ScriptActions
	submitMu         sync.Mutex
	lastSubmitByHost map[string]time.Time
	now              func() time.Time

	// Caches for filesystem-derived views. Evidence directories are written once
	// and never edited, so a (path, size, mtime) key is a safe identity.
	evCache    sync.Map // evidence dir -> evSummaryEntry
	agentMu    sync.Mutex
	agentAt    time.Time
	agentView  *agentView
	transcript sync.Map // transcript path -> transcriptEntry
}

// RequestSubmitter is the complete authority the dashboard receives for creating
// a QA request. The implementation owns validation, persistence and preemption.
type RequestSubmitter interface {
	SubmitUserRequest(ctx context.Context, situation string) (featureID string, jobID int64, err error)
}

// SetRequestSubmitter enables POST /api/requests. Standalone read-only servers
// intentionally leave it unset and return 503 for submissions.
func (s *Server) SetRequestSubmitter(submitter RequestSubmitter) { s.requestSubmitter = submitter }

// SetDevDir serves the page assets (todo/results/scripts/activity/help/report
// HTML, theme.css, app.js) from dir instead of the embedded copies.
func (s *Server) SetDevDir(dir string) { s.devDir = dir }

func New(cfg *config.Config, st *store.Store, evidenceRoot string) *Server {
	return &Server{
		cfg: cfg, st: st, evRoot: evidenceRoot,
		lastSubmitByHost: make(map[string]time.Time),
		now:              time.Now,
	}
}

// staticAsset is one embedded page or stylesheet plus its content hash, so a
// browser revalidating with If-None-Match gets a 304 instead of the body.
type staticAsset struct {
	name, contentType string
	body              []byte
	etag              string
}

func newStatic(name, contentType string, body []byte) staticAsset {
	sum := sha256.Sum256(body)
	return staticAsset{name: name, contentType: contentType, body: body, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
}

func (s *Server) serveStatic(a staticAsset) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", a.contentType)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if s.devDir != "" {
			if b, err := os.ReadFile(filepath.Join(s.devDir, a.name)); err == nil {
				w.Header().Set("Cache-Control", "no-store")
				_, _ = w.Write(b)
				return
			}
		}
		// no-cache (not no-store): the browser keeps a copy but must revalidate,
		// so a rebuilt binary is picked up immediately and an unchanged one costs
		// a 304.
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("ETag", a.etag)
		if r.Header.Get("If-None-Match") == a.etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		_, _ = w.Write(a.body)
	}
}

func (s *Server) Handler() http.Handler {
	const html = "text/html; charset=utf-8"
	mux := http.NewServeMux()
	// "/" is the action queue (할 일). Old bookmarks that expected the
	// verification matrix here still load a page; the matrix moved to /results.
	home := s.serveStatic(newStatic("todo.html", html, todoHTML))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		home(w, r)
	})
	mux.HandleFunc("/results", s.serveStatic(newStatic("results.html", html, resultsHTML)))
	mux.HandleFunc("/help", s.serveStatic(newStatic("help.html", html, helpHTML)))
	mux.HandleFunc("/activity", s.serveStatic(newStatic("activity.html", html, activityHTML)))
	mux.HandleFunc("/report", s.serveStatic(newStatic("report.html", html, reportHTML)))
	mux.HandleFunc("/scripts", s.serveStatic(newStatic("scripts.html", html, scriptsHTML)))
	mux.HandleFunc("/ui/theme.css", s.serveStatic(newStatic("theme.css", "text/css; charset=utf-8", themeCSS)))
	mux.HandleFunc("/ui/app.js", s.serveStatic(newStatic("app.js", "text/javascript; charset=utf-8", appJS)))

	mux.HandleFunc("/api/todo", getOnly(s.todo))
	mux.HandleFunc("/api/overview", getOnly(s.overview))
	mux.HandleFunc("/api/verification", getOnly(s.verification))
	mux.HandleFunc("/api/requests", s.requests)
	mux.HandleFunc("/api/run/detail", getOnly(s.runDetail))
	mux.HandleFunc("/api/run/steps", getOnly(s.runSteps))
	mux.HandleFunc("/api/scripts", getOnly(s.scripts))
	mux.HandleFunc("/api/script", getOnly(s.script))
	mux.HandleFunc("/api/script/run", s.scriptRun)
	mux.HandleFunc("/api/script/approve", s.scriptApprove)
	mux.HandleFunc("/api/script/reject", s.scriptReject)
	mux.HandleFunc("/api/schedule/window", s.scheduleWindow)
	mux.HandleFunc("/api/supervisor", getOnly(s.supervisor))
	mux.HandleFunc("/api/agent/latest", getOnly(s.agentLatest))
	// Evidence is already secret-redacted by the runner/agent adapter; serve it read-only.
	mux.Handle("/evidence/", http.StripPrefix("/evidence/", http.FileServer(http.Dir(s.evRoot))))
	return mux
}

// getOnly rejects writes to read endpoints so a mistaken POST cannot look like
// it did something.
func getOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			writeAPIError(w, http.StatusMethodNotAllowed, "지원하지 않는 요청 방식입니다")
			return
		}
		h(w, r)
	}
}

// ListenAndServe blocks until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// ---- evidence path helpers -------------------------------------------------

// rel converts an absolute evidence path to the slash-separated form the
// dashboard links under /evidence/. Paths outside the root become "".
func (s *Server) rel(abs string) string {
	if abs == "" {
		return ""
	}
	r, err := filepath.Rel(s.evRoot, abs)
	if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(r)
}

// safeJoin resolves a dashboard-supplied relative evidence path and refuses
// anything that would escape the evidence root.
func (s *Server) safeJoin(rel string) (string, bool) {
	if rel == "" || strings.Contains(rel, "..") {
		return "", false
	}
	p := filepath.Join(s.evRoot, filepath.FromSlash(rel))
	if !strings.HasPrefix(p, filepath.Clean(s.evRoot)+string(filepath.Separator)) {
		return "", false
	}
	return p, true
}
