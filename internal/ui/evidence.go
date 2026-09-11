package ui

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// ---- run evidence: steps, detail bundle, and the cached per-run summary ------

func (s *Server) runSteps(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("dir")
	dir, ok := s.safeJoin(rel)
	if !ok {
		http.Error(w, "bad dir", http.StatusBadRequest)
		return
	}
	b, err := os.ReadFile(filepath.Join(dir, "steps.json"))
	if err != nil {
		http.Error(w, "steps.json not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// evSummary is what the verification table shows per run without loading the
// evidence itself: counts plus whether a screenshot exists.
type evSummary struct {
	Steps, Requests, ConsoleErrs int
	Screenshot                   bool
}

type evSummaryEntry struct {
	size  int64
	mtime int64
	sum   evSummary
}

// evidenceSummary counts what a run dir holds. A finished run's directory is
// immutable, so the result is cached against steps.json's (size, mtime); the
// verification page polls every few seconds and used to re-parse every
// network.json on each poll.
func (s *Server) evidenceSummary(dir, rel string) (steps, requests, consoleErrs int, screenshot string) {
	if dir == "" {
		return
	}
	stepsPath := filepath.Join(dir, "steps.json")
	st, err := os.Stat(stepsPath)
	if err != nil {
		return
	}
	if v, ok := s.evCache.Load(dir); ok {
		e := v.(evSummaryEntry)
		if e.size == st.Size() && e.mtime == st.ModTime().UnixNano() {
			return e.sum.Steps, e.sum.Requests, e.sum.ConsoleErrs, screenshotRel(e.sum.Screenshot, rel)
		}
	}
	var sum evSummary
	if b, err := os.ReadFile(stepsPath); err == nil {
		var sf struct {
			Steps []json.RawMessage `json:"steps"`
		}
		if json.Unmarshal(b, &sf) == nil {
			sum.Steps = len(sf.Steps)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "network.json")); err == nil {
		var ev []json.RawMessage
		if json.Unmarshal(b, &ev) == nil {
			sum.Requests = len(ev)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "console.json")); err == nil {
		var ev []struct {
			Level string `json:"level"`
		}
		if json.Unmarshal(b, &ev) == nil {
			for _, e := range ev {
				if e.Level == "error" || e.Level == "exception" {
					sum.ConsoleErrs++
				}
			}
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "screenshot.png")); err == nil {
		sum.Screenshot = true
	}
	s.evCache.Store(dir, evSummaryEntry{size: st.Size(), mtime: st.ModTime().UnixNano(), sum: sum})
	return sum.Steps, sum.Requests, sum.ConsoleErrs, screenshotRel(sum.Screenshot, rel)
}

func screenshotRel(has bool, rel string) string {
	if has && rel != "" {
		return rel + "/screenshot.png"
	}
	return ""
}

// runDetail bundles steps + network + console + screenshots for one run dir.
func (s *Server) runDetail(w http.ResponseWriter, r *http.Request) {
	dir, ok := s.safeJoin(r.URL.Query().Get("dir"))
	if !ok {
		http.Error(w, "bad dir", http.StatusBadRequest)
		return
	}
	out := map[string]any{}
	if b, err := os.ReadFile(filepath.Join(dir, "steps.json")); err == nil {
		out["steps"] = json.RawMessage(b)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "result.json")); err == nil {
		var rs struct {
			Environment string `json:"environment"`
		}
		if json.Unmarshal(b, &rs) == nil && rs.Environment != "" {
			out["environment"] = rs.Environment
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "network.json")); err == nil {
		var ev []struct {
			Method string `json:"method"`
			URL    string `json:"url"`
			Status int    `json:"status"`
			Mime   string `json:"mime_type"`
			Failed bool   `json:"failed"`
			Error  string `json:"error"`
		}
		if json.Unmarshal(b, &ev) == nil {
			type nv struct {
				Method string `json:"method"`
				URL    string `json:"url"`
				Status int    `json:"status"`
				Mime   string `json:"mime"`
				Failed bool   `json:"failed"`
				Error  string `json:"error,omitempty"`
			}
			keep := []nv{}
			for _, e := range ev {
				// skip static assets unless they failed; keep documents, API calls and anything not 2xx
				static := strings.Contains(e.Mime, "image") || strings.Contains(e.Mime, "font") || strings.Contains(e.Mime, "css") || strings.HasSuffix(e.URL, ".js") || strings.Contains(e.URL, "/assets/")
				if !e.Failed && e.Status >= 200 && e.Status < 300 && static {
					continue
				}
				keep = append(keep, nv{e.Method, clip(e.URL, 200), e.Status, e.Mime, e.Failed, e.Error})
			}
			out["network"] = keep
			out["network_total"] = len(ev)
		}
	}
	if b, err := os.ReadFile(filepath.Join(dir, "console.json")); err == nil {
		out["console"] = json.RawMessage(b)
	}
	shots := []string{}
	entries, _ := os.ReadDir(dir)
	rel := s.rel(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".png") {
			shots = append(shots, rel+"/"+e.Name())
		}
	}
	out["screenshots"] = shots
	out["dir"] = rel
	// A run directory never changes once written, so the bundle can be cached hard.
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	_ = json.NewEncoder(w).Encode(out)
}
