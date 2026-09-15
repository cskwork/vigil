package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Runtime struct {
	Handler http.Handler
	Busy    func() bool
	Close   func()
}
type Factory func(Project) (Runtime, error)
type Console struct {
	mu       sync.Mutex
	path     string
	registry Registry
	factory  Factory
	runtimes map[string]Runtime
}

func New(path string, initial Registry, factory Factory) (*Console, error) {
	r, e := load(path, initial)
	if e != nil {
		return nil, e
	}
	return &Console{path: path, registry: r, factory: factory, runtimes: map[string]Runtime{}}, nil
}
func (c *Console) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.runtimes {
		if r.Close != nil {
			r.Close()
		}
	}
}
func response(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func failure(w http.ResponseWriter, status int, message string) {
	response(w, status, map[string]string{"error": message})
}
func sameOrigin(r *http.Request) bool {
	if r.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	if r.Header.Get("Origin") == "" {
		return true
	}
	u, e := url.Parse(r.Header.Get("Origin"))
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return e == nil && u.User == nil && u.Scheme == scheme && strings.EqualFold(u.Host, r.Host) && (u.Path == "" || u.Path == "/")
}
func (c *Console) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Lock across dispatch: updates cannot close a store while its handler uses it.
	c.mu.Lock()
	defer c.mu.Unlock()
	if r.URL.Path == "/api/projects" {
		c.projects(w, r)
		return
	}
	id := r.URL.Query().Get("project")
	if id == "" {
		id = c.registry.DefaultProject
	}
	var p *Project
	for i := range c.registry.Projects {
		if c.registry.Projects[i].ID == id {
			p = &c.registry.Projects[i]
			break
		}
	}
	if p == nil {
		failure(w, 404, "프로젝트를 찾을 수 없습니다")
		return
	}
	rt, ok := c.runtimes[id]
	if !ok {
		var e error
		rt, e = c.factory(*p)
		if e != nil {
			failure(w, 500, "프로젝트를 열지 못했습니다: "+e.Error())
			return
		}
		c.runtimes[id] = rt
	}
	rt.Handler.ServeHTTP(w, r)
}
func (c *Console) projects(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		response(w, 200, c.registry)
		return
	}
	if r.Method != "POST" && r.Method != "PUT" {
		w.Header().Set("Allow", "GET, POST, PUT")
		failure(w, 405, "지원하지 않는 요청 방식입니다")
		return
	}
	if !sameOrigin(r) {
		failure(w, 403, "다른 출처에서는 프로젝트를 변경할 수 없습니다")
		return
	}
	mt, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if e != nil || mt != "application/json" {
		failure(w, 415, "JSON 형식으로 전송하세요")
		return
	}
	var p Project
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	if dec.Decode(&p) != nil || dec.Decode(&struct{}{}) != io.EOF {
		failure(w, 400, "프로젝트 정보 형식이 올바르지 않습니다")
		return
	}
	if e = p.Validate(); e != nil {
		failure(w, 422, e.Error())
		return
	}
	idx := -1
	for i, v := range c.registry.Projects {
		if v.ID == p.ID {
			idx = i
		}
	}
	if r.Method == "POST" && idx >= 0 {
		failure(w, 409, "이미 등록된 프로젝트 ID입니다")
		return
	}
	if r.Method == "PUT" && idx < 0 {
		failure(w, 404, "프로젝트를 찾을 수 없습니다")
		return
	}
	if idx >= 0 && p.Revision != c.registry.Projects[idx].Revision {
		failure(w, 409, "다른 화면에서 변경되었습니다. 새로고침 후 다시 수정하세요")
		return
	}
	if rt, ok := c.runtimes[p.ID]; ok && rt.Busy != nil && rt.Busy() {
		failure(w, 409, "검증 실행 중에는 사이트를 변경할 수 없습니다. 완료 후 다시 저장하세요")
		return
	}
	// Removing a site can strand queued runs; this console supports additions and
	// edits, not deletion. Keep the persistent site identity when changing a URL.
	if idx >= 0 {
		for _, old := range c.registry.Projects[idx].Sites {
			found := false
			for _, s := range p.Sites {
				if s.ID == old.ID {
					found = true
				}
			}
			if !found {
				failure(w, 422, "등록된 사이트는 삭제할 수 없습니다. 이름과 URL을 수정하세요")
				return
			}
		}
	}
	next := c.registry
	next.Projects = append([]Project(nil), next.Projects...)
	if idx < 0 {
		if len(next.Projects) >= 100 {
			failure(w, 422, "프로젝트는 100개까지 등록할 수 있습니다")
			return
		}
		p.Revision = 1
		next.Projects = append(next.Projects, p)
	} else {
		p.Revision++
		next.Projects[idx] = p
	}
	if e = save(c.path, next); e != nil {
		failure(w, 500, "설정을 저장하지 못했습니다")
		return
	}
	if rt, ok := c.runtimes[p.ID]; ok {
		if rt.Close != nil {
			rt.Close()
		}
		delete(c.runtimes, p.ID)
	}
	c.registry = next
	response(w, 200, p)
}
func Listen(ctx context.Context, addr string, h http.Handler) error {
	s := &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { <-ctx.Done(); _ = s.Close() }()
	e := s.ListenAndServe()
	if e == http.ErrServerClosed {
		return nil
	}
	return fmt.Errorf("admin server: %w", e)
}
