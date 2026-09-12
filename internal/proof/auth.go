package proof

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// LoginUser is operator provisioned. Neither team nor actor comes from a login request.
type LoginUser struct {
	Name        string `json:"name"`
	Team        string `json:"team"`
	PasswordEnv string `json:"password_env"`
}
type principal struct {
	Actor, Team, CSRF string
}
type principalKey struct{}

func identity(r *http.Request, fallback string) principal {
	if p, ok := r.Context().Value(principalKey{}).(principal); ok {
		return p
	}
	return principal{Actor: fallback}
}
func apiError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

type loginSession struct {
	principal
	Expires time.Time
}
type loginLimit struct {
	Count int
	Until time.Time
}
type sessionAuth struct {
	mu       sync.Mutex
	users    map[string]LoginUser
	sessions map[[32]byte]loginSession
	limits   map[string]loginLimit
	origin   string
	secure   bool
}

func newSessionAuth(cfg HTTPConfig) (*sessionAuth, error) {
	if len(cfg.Users) == 0 {
		return nil, nil
	}
	if cfg.LocalOperator || cfg.GatewayToken != "" {
		return nil, fmt.Errorf("choose one authentication mode")
	}
	u, e := url.Parse(cfg.PublicOrigin)
	if e != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("public_origin must be an exact origin")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, fmt.Errorf("session login requires HTTPS outside literal loopback")
	}
	a := &sessionAuth{users: map[string]LoginUser{}, sessions: map[[32]byte]loginSession{}, limits: map[string]loginLimit{}, origin: cfg.PublicOrigin, secure: u.Scheme == "https"}
	for _, user := range cfg.Users {
		if user.Name == "" || user.Team == "" || len(os.Getenv(user.PasswordEnv)) < 16 {
			return nil, fmt.Errorf("login user needs name, team and a password environment value of at least 16 characters")
		}
		if _, ok := a.users[user.Name]; ok {
			return nil, fmt.Errorf("duplicate login user")
		}
		a.users[user.Name] = user
	}
	return a, nil
}
func (a *sessionAuth) wrap(next http.Handler) http.Handler {
	if a == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		u, _ := url.Parse(a.origin)
		if r.Host != u.Host {
			apiError(w, 403, "등록된 서비스 주소로 접속해 주세요.")
			return
		}
		if r.URL.Path == "/api/proof/login" && r.Method == "POST" {
			if r.Header.Get("Origin") != a.origin {
				apiError(w, 403, "로그인 요청 출처가 다릅니다.")
				return
			}
			host, _, _ := net.SplitHostPort(r.RemoteAddr)
			a.mu.Lock()
			defer a.mu.Unlock()
			now := time.Now()
			for k, v := range a.limits {
				if now.After(v.Until) {
					delete(a.limits, k)
				}
			}
			for k, v := range a.sessions {
				if now.After(v.Expires) {
					delete(a.sessions, k)
				}
			}
			limit := a.limits[host]
			if limit.Count >= 5 && now.Before(limit.Until) {
				apiError(w, 429, "로그인 시도가 많습니다. 1분 뒤 다시 시도해 주세요.")
				return
			}
			var in struct {
				Name     string `json:"name"`
				Password string `json:"password"`
			}
			d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096))
			d.DisallowUnknownFields()
			if d.Decode(&in) != nil {
				apiError(w, 400, "로그인 입력을 확인해 주세요.")
				return
			}
			var tail any
			if d.Decode(&tail) != io.EOF {
				apiError(w, 400, "로그인 입력을 확인해 주세요.")
				return
			}
			user, exists := a.users[in.Name]
			want := sha256.Sum256([]byte(os.Getenv(user.PasswordEnv)))
			got := sha256.Sum256([]byte(in.Password))
			if subtle.ConstantTimeCompare(want[:], got[:]) != 1 || !exists {
				limit.Count++
				limit.Until = now.Add(time.Minute)
				a.limits[host] = limit
				apiError(w, 401, "계정 또는 비밀번호가 올바르지 않습니다.")
				return
			}
			delete(a.limits, host)
			if len(a.sessions) >= 10000 {
				apiError(w, 429, "접속이 많습니다. 잠시 후 다시 시도해 주세요.")
				return
			}
			if old, e := r.Cookie("proof_session"); e == nil {
				delete(a.sessions, sha256.Sum256([]byte(old.Value)))
			}
			token := id() + id()
			a.sessions[sha256.Sum256([]byte(token))] = loginSession{principal: principal{Actor: user.Name, Team: user.Team, CSRF: id()}, Expires: now.Add(8 * time.Hour)}
			http.SetCookie(w, &http.Cookie{Name: "proof_session", Value: token, Path: "/", HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode, MaxAge: 8 * 60 * 60})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"signed_in":true}`))
			return
		}
		var session loginSession
		valid := false
		if cookie, e := r.Cookie("proof_session"); e == nil {
			a.mu.Lock()
			session, valid = a.sessions[sha256.Sum256([]byte(cookie.Value))]
			a.mu.Unlock()
			valid = valid && time.Now().Before(session.Expires)
		}
		if !valid {
			if r.Method == "GET" && (r.URL.Path == "/" || strings.HasPrefix(r.URL.Path, "/checks/") || r.URL.Path == "/ui/app.js" || r.URL.Path == "/ui/style.css") {
				UI().ServeHTTP(w, r)
				return
			}
			apiError(w, 401, "로그인이 필요합니다.")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("Origin") != a.origin || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Proof-CSRF")), []byte(session.CSRF)) != 1 {
				apiError(w, 403, "로그인 상태와 요청 출처를 확인해 주세요.")
				return
			}
		}
		if r.URL.Path == "/api/proof/logout" && r.Method == "POST" {
			cookie, _ := r.Cookie("proof_session")
			a.mu.Lock()
			delete(a.sessions, sha256.Sum256([]byte(cookie.Value)))
			a.mu.Unlock()
			http.SetCookie(w, &http.Cookie{Name: "proof_session", Path: "/", MaxAge: -1, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteStrictMode})
			_, _ = w.Write([]byte(`{"signed_out":true}`))
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), principalKey{}, session.principal)))
	})
}

func teamAllowed(t Target, team string) bool {
	if team == "" {
		return true
	} // explicit local operator or legacy fixed gateway
	if len(t.Teams) == 0 {
		return true
	}
	for _, v := range t.Teams {
		if v == team {
			return true
		}
	}
	return false
}
func (s *Service) authorizeResource(r *http.Request) bool {
	p := identity(r, "")
	if p.Team == "" {
		return true
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/proof/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 || parts[1] == "" {
		return true
	}
	var c *Check
	var e error
	switch parts[0] {
	case "checks":
		c, e = s.Repo.GetCheck(r.Context(), parts[1])
	case "attempts":
		var a *Attempt
		a, e = s.Repo.Attempt(r.Context(), parts[1])
		if e == nil {
			c, e = s.Repo.GetCheck(r.Context(), a.CheckID)
		}
	default:
		return true
	}
	return e == nil && c.Team == p.Team && teamAllowed(s.Registry.Targets[c.TargetRef], p.Team)
}
