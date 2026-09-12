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
	Users         []LoginUser
	PublicOrigin  string
	LocalOperator bool
	GatewayToken  string
	GatewayActor  string
	UI            http.Handler
}

func (s *Service) Handler(cfg HTTPConfig) (http.Handler, error) {
	auth, err := newSessionAuth(cfg)
	if err != nil {
		return nil, err
	}
	if auth == nil && !cfg.LocalOperator && (len(cfg.GatewayToken) < 32 || cfg.GatewayActor == "") {
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
	replyStatus := func(w http.ResponseWriter, r *http.Request, status int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"`+Hash(v)+`"`)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
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
		p := identity(r, actor)
		token := csrf
		if p.CSRF != "" {
			token = p.CSRF
		}
		if auth == nil {
			reply(w, r, map[string]string{"actor": p.Actor, "csrf": token})
			return
		}
		reply(w, r, map[string]any{"actor": p.Actor, "team": p.Team, "csrf": token, "login": auth != nil})
	})
	mux.HandleFunc("GET /api/proof/registry", func(w http.ResponseWriter, r *http.Request) {
		targets := map[string]Target{}
		for k, t := range s.Registry.Targets {
			if !teamAllowed(t, identity(r, actor).Team) {
				continue
			}
			registryHash := Hash(t)
			b, _ := json.Marshal(t)
			var copy Target
			json.Unmarshal(b, &copy)
			for id, p := range copy.Personas {
				p.Secrets = nil
				p.Setup = nil
				copy.Personas[id] = p
			}
			copy.RegistryHash = registryHash
			targets[k] = copy
		}
		reply(w, r, map[string]any{"targets": targets})
	})
	mux.HandleFunc("POST /api/proof/checks", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			TargetRef      string `json:"target_ref"`
			Request        string `json:"request"`
			URL            string `json:"url,omitempty"`
			IdempotencyKey string `json:"idempotency_key,omitempty"`
		}
		if !decode(w, r, &in) {
			return
		}
		if in.TargetRef == "" && in.URL != "" {
			for key, t := range s.Registry.Targets {
				if in.URL == t.BaseURL {
					in.TargetRef = key
					break
				}
			}
		}
		target, ok := s.Registry.Targets[in.TargetRef]
		if !ok {
			fail(w, fmt.Errorf("unknown registered target"))
			return
		}
		if in.URL != "" && in.URL != target.BaseURL {
			apiError(w, http.StatusUnprocessableEntity, "이 주소는 아직 확인 대상으로 등록되지 않았습니다.")
			return
		}
		p := identity(r, actor)
		if !teamAllowed(target, p.Team) {
			apiError(w, 403, "이 대상을 확인할 권한이 없습니다.")
			return
		}
		if len(strings.TrimSpace(in.Request)) < 3 || len(in.Request) > 10000 {
			apiError(w, http.StatusBadRequest, "확인할 내용은 3자 이상 10,000자 이하로 적어 주세요.")
			return
		}
		if len(in.IdempotencyKey) > 128 {
			apiError(w, http.StatusBadRequest, "중복 방지 키가 너무 깁니다.")
			return
		}
		c, e := s.Repo.Create(r.Context(), in.TargetRef, in.Request, p.Actor, p.Team, in.IdempotencyKey)
		if e != nil {
			fail(w, e)
			return
		}
		s.Wake()
		replyStatus(w, r, http.StatusCreated, c)
	})
	mux.HandleFunc("GET /api/proof/checks", func(w http.ResponseWriter, r *http.Request) {
		cs, e := s.Repo.List(r.Context(), r.URL.Query().Get("cursor"), identity(r, actor).Team)
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
		if _, e = s.Registry.Compile(c.TargetRef, in.Contract, sourceText(*c)); e != nil {
			fail(w, e)
			return
		}
		if requiredCriteriaRemoved(c.Draft, in.Contract) && strings.TrimSpace(in.RemovalReason) == "" {
			fail(w, fmt.Errorf("required criterion removal needs a reason"))
			return
		}
		c, e = s.Repo.Patch(r.Context(), c.ID, in.RowVersion, in.Contract, identity(r, actor).Actor, in.RemovalReason)
		if e != nil {
			fail(w, e)
			return
		}
		reply(w, r, c)
	})
	mux.HandleFunc("POST /api/proof/checks/{id}/plan", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			RowVersion int    `json:"row_version"`
			Answer     string `json:"answer"`
		}
		if !decode(w, r, &in) {
			return
		}
		c, e := s.Repo.Replan(r.Context(), r.PathValue("id"), in.RowVersion, in.Answer)
		if e != nil {
			fail(w, e)
			return
		}
		s.Wake()
		reply(w, r, c)
	})
	mux.HandleFunc("POST /api/proof/checks/{id}/attempts", func(w http.ResponseWriter, r *http.Request) {
		var in Approval
		if !decode(w, r, &in) {
			return
		}
		if in.IdempotencyKey == "" || len(in.IdempotencyKey) > 128 {
			apiError(w, http.StatusBadRequest, "실행 중복 방지 키가 필요합니다.")
			return
		}
		a, _, e := s.Repo.Approve(r.Context(), r.PathValue("id"), in, identity(r, actor).Actor, s.Registry)
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
		if len(in.Reason) > 500 {
			apiError(w, http.StatusBadRequest, "판단 사유는 500자 이하로 적어 주세요.")
			return
		}
		if e := s.Repo.Disposition(r.Context(), r.PathValue("id"), in.Disposition, identity(r, actor).Actor, in.Reason); e != nil {
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
				format := r.URL.Query().Get("format")
				image := format == "image" || format == "before"
				if image {
					artifact = ev.Screenshot
					if format == "before" {
						artifact = ev.BeforeScreenshot
					}
					if artifact == "" {
						http.Error(w, "image unavailable", 410)
						return
					}
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
	return auth.wrap(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
		}
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		if identity(r, actor).Team != "" {
			if !s.authorizeResource(r) {
				apiError(w, 403, "같은 팀의 확인 기록만 열 수 있습니다.")
				return
			}
		} else if cfg.LocalOperator {
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
		if identity(r, actor).Team == "" && r.Method != "GET" && r.Method != "HEAD" {
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
	})), nil
}

func requiredCriteriaRemoved(before, after Contract) bool {
	required := map[string]bool{}
	for _, criterion := range after.Criteria {
		required[criterion.ID] = criterion.Required
	}
	for _, criterion := range before.Criteria {
		if criterion.Required && !required[criterion.ID] {
			return true
		}
	}
	return false
}
