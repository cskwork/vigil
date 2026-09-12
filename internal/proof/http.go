package proof

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// HTTPConfig uses an explicit loopback operator or a gateway bearer secret.
// Gateway access requires a fixed authenticated actor, never client identity headers.
type HTTPConfig struct {
	LocalOperator bool
	GatewayToken  string
	GatewayActor  string
	UI            http.Handler
}

func (s *Service) Handler(cfg HTTPConfig) (http.Handler, error) {
	if !cfg.LocalOperator && (len(cfg.GatewayToken) < 32 || cfg.GatewayActor == "") {
		return nil, fmt.Errorf("external proof access requires a gateway token and authenticated actor")
	}
	csrf := id()
	mux := http.NewServeMux()
	reply := func(w http.ResponseWriter, r *http.Request, v any) {
		w.Header().Set("Content-Type", "application/json")
		etag := `"` + Hash(v) + `"`
		w.Header().Set("ETag", etag)
		if r.Method == "GET" && r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		json.NewEncoder(w).Encode(v)
	}
	fail := func(w http.ResponseWriter, e error) {
		status := 422
		switch {
		case errors.Is(e, ErrNotFound):
			status = 404
		case errors.Is(e, ErrConflict):
			status = 409
		case errors.Is(e, ErrBusy):
			status = 429
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]string{"error": e.Error()})
	}
	decode := func(w http.ResponseWriter, r *http.Request, v any) bool {
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
		d := json.NewDecoder(r.Body)
		d.DisallowUnknownFields()
		if e := d.Decode(v); e != nil {
			http.Error(w, `{"error":"invalid JSON"}`, 400)
			return false
		}
		var tail any
		if d.Decode(&tail) != io.EOF {
			http.Error(w, `{"error":"one JSON value required"}`, 400)
			return false
		}
		return true
	}
	actor := "local-operator"
	if !cfg.LocalOperator {
		actor = cfg.GatewayActor
	}
	mux.HandleFunc("GET /api/proof/session", func(w http.ResponseWriter, r *http.Request) {
		reply(w, r, map[string]string{"actor": actor, "csrf": csrf})
	})
	mux.HandleFunc("GET /api/proof/registry", func(w http.ResponseWriter, r *http.Request) {
		targets := map[string]Target{}
		for k, t := range s.Registry.Targets {
			registryHash := Hash(t)
			b, _ := json.Marshal(t)
			var copy Target
			json.Unmarshal(b, &copy)
			for id, p := range copy.Personas {
				p.Secrets = nil
				copy.Personas[id] = p
			}
			copy.RegistryHash = registryHash
			targets[k] = copy
		}
		reply(w, r, map[string]any{"targets": targets})
	})
	mux.HandleFunc("POST /api/proof/checks", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TargetRef string `json:"target_ref"`
			Request   string `json:"request"`
		}
		if !decode(w, r, &in) {
			return
		}
		if _, ok := s.Registry.Targets[in.TargetRef]; !ok {
			fail(w, fmt.Errorf("unknown registered target"))
			return
		}
		if len(strings.TrimSpace(in.Request)) < 3 || len(in.Request) > 10000 {
			fail(w, fmt.Errorf("request must contain 3 to 10000 characters"))
			return
		}
		c, e := s.Repo.Create(r.Context(), in.TargetRef, in.Request, actor)
		if e != nil {
			fail(w, e)
			return
		}
		s.Wake()
		w.WriteHeader(201)
		reply(w, r, c)
	})
	mux.HandleFunc("GET /api/proof/checks", func(w http.ResponseWriter, r *http.Request) {
		cs, e := s.Repo.List(r.Context(), r.URL.Query().Get("cursor"))
		if e != nil {
			fail(w, e)
			return
		}
		cursor := ""
		if len(cs) > 50 {
			cs = cs[:50]
			cursor = cs[49].ID
		}
		reply(w, r, map[string]any{"checks": cs, "next_cursor": cursor})
	})
	mux.HandleFunc("GET /api/proof/checks/{id}", func(w http.ResponseWriter, r *http.Request) {
		c, e := s.Repo.GetCheck(r.Context(), r.PathValue("id"))
		if e != nil {
			fail(w, e)
			return
		}
		as, e := s.Repo.Attempts(r.Context(), c.ID)
		if e != nil {
			fail(w, e)
			return
		}
		for i := range as {
			as[i] = *s.publicAttempt(&as[i])
		}
		reply(w, r, map[string]any{"check": c, "attempts": as})
	})
	mux.HandleFunc("PATCH /api/proof/checks/{id}/contract", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RowVersion    int      `json:"row_version"`
			Contract      Contract `json:"contract"`
			RemovalReason string   `json:"removal_reason"`
		}
		if !decode(w, r, &in) {
			return
		}
		c, e := s.Repo.GetCheck(r.Context(), r.PathValue("id"))
		if e != nil {
			fail(w, e)
			return
		}
		if _, e = s.Registry.Compile(c.TargetRef, in.Contract, c.Request); e != nil {
			fail(w, e)
			return
		}
		removed := false
		ids := map[string]bool{}
		for _, cr := range in.Contract.Criteria {
			ids[cr.ID] = true
		}
		for _, cr := range c.Draft.Criteria {
			if cr.Required && !ids[cr.ID] {
				removed = true
			}
		}
		if removed && strings.TrimSpace(in.RemovalReason) == "" {
			fail(w, fmt.Errorf("required criterion removal needs a reason"))
			return
		}
		c, e = s.Repo.Patch(r.Context(), c.ID, in.RowVersion, in.Contract, actor, in.RemovalReason)
		if e != nil {
			fail(w, e)
			return
		}
		reply(w, r, c)
	})
	mux.HandleFunc("POST /api/proof/checks/{id}/attempts", func(w http.ResponseWriter, r *http.Request) {
		var in Approval
		if !decode(w, r, &in) {
			return
		}
		if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 128 {
			fail(w, fmt.Errorf("idempotency_key required, maximum 128 characters"))
			return
		}
		a, _, e := s.Repo.Approve(r.Context(), r.PathValue("id"), in, actor, s.Registry)
		if e != nil {
			fail(w, e)
			return
		}
		s.Wake()
		reply(w, r, s.publicAttempt(a))
	})
	mux.HandleFunc("GET /api/proof/attempts/{id}", func(w http.ResponseWriter, r *http.Request) {
		a, e := s.Repo.Attempt(r.Context(), r.PathValue("id"))
		if e != nil {
			fail(w, e)
			return
		}
		reply(w, r, s.publicAttempt(a))
	})
	mux.HandleFunc("POST /api/proof/attempts/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		if e := s.Cancel(r.Context(), r.PathValue("id")); e != nil {
			fail(w, e)
			return
		}
		reply(w, r, map[string]bool{"cancel_requested": true})
	})
	mux.HandleFunc("POST /api/proof/attempts/{id}/disposition", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Disposition string `json:"disposition"`
			Reason      string `json:"reason"`
		}
		if !decode(w, r, &in) {
			return
		}
		if e := s.Repo.Disposition(r.Context(), r.PathValue("id"), in.Disposition, actor, in.Reason); e != nil {
			fail(w, e)
			return
		}
		reply(w, r, map[string]string{"disposition": in.Disposition})
	})
	mux.HandleFunc("GET /api/proof/attempts/{id}/evidence/{evidence}", func(w http.ResponseWriter, r *http.Request) {
		a, e := s.Repo.Attempt(r.Context(), r.PathValue("id"))
		if e != nil {
			fail(w, e)
			return
		}
		for _, cr := range a.Results {
			for _, ev := range cr.Evidence {
				if ev.ID != r.PathValue("evidence") {
					continue
				}
				if ev.Artifact == "" || time.Since(ev.At) > s.RawRetention {
					http.Error(w, "evidence expired or unavailable", 410)
					return
				}
				manifestInfo, manifestErr := os.Lstat(filepath.Join(s.EvidenceDir, a.ID, filepath.Base(ev.Artifact)))
				if manifestErr != nil || !manifestInfo.Mode().IsRegular() {
					http.Error(w, "evidence unavailable", 410)
					return
				}
				artifact := ev.Artifact
				image := r.URL.Query().Get("format") == "image"
				if image {
					if ev.Screenshot == "" {
						http.Error(w, "image unavailable", 410)
						return
					}
					artifact = ev.Screenshot
				}
				p := filepath.Join(s.EvidenceDir, a.ID, filepath.Base(artifact))
				fi, e := os.Lstat(p)
				if e != nil || !fi.Mode().IsRegular() {
					http.Error(w, "evidence unavailable", 410)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Content-Disposition", `attachment; filename="evidence.json"`)
				if image {
					w.Header().Set("Content-Type", "image/png")
					w.Header().Set("Content-Disposition", `inline; filename="evidence.png"`)
				}
				http.ServeFile(w, r, p)
				return
			}
		}
		http.NotFound(w, r)
	})
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") || cfg.UI == nil {
			http.NotFound(w, r)
			return
		}
		cfg.UI.ServeHTTP(w, r)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		if cfg.LocalOperator {
			host, _, e := net.SplitHostPort(r.RemoteAddr)
			ip := net.ParseIP(host)
			reqhost := r.Host
			if h, _, e := net.SplitHostPort(reqhost); e == nil {
				reqhost = h
			}
			if e != nil || ip == nil || !ip.IsLoopback() || (reqhost != "localhost" && net.ParseIP(reqhost) == nil) || (net.ParseIP(reqhost) != nil && !net.ParseIP(reqhost).IsLoopback()) {
				http.Error(w, "loopback operator only", 403)
				return
			}
		} else {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if subtle.ConstantTimeCompare([]byte(got), []byte(cfg.GatewayToken)) != 1 {
				http.Error(w, "authentication required", 403)
				return
			}
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			origin := r.Header.Get("Origin")
			scheme := "http"
			if r.TLS != nil {
				scheme = "https"
			}
			if origin != "" && origin != scheme+"://"+r.Host {
				http.Error(w, "origin denied", 403)
				return
			}
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Proof-CSRF")), []byte(csrf)) != 1 {
				http.Error(w, "CSRF token required", 403)
				return
			}
		}
		mux.ServeHTTP(w, r)
	}), nil
}
