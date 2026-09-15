// Package admin owns the local multi-project console registry. Runtime configs
// are immutable snapshots; a URL edit never changes a running test's target.
package admin

import (
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"vigil/internal/config"
)

type Site struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	URL          string   `json:"url"`
	AllowedHosts []string `json:"allowed_hosts"`
	ReadOnly     bool     `json:"read_only"`
}
type Project struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DefaultSite string `json:"default_site"`
	Sites       []Site `json:"sites"`
	Revision    int    `json:"revision"`
}
type Registry struct {
	DefaultProject string    `json:"default_project"`
	Projects       []Project `json:"projects"`
}

var identifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func (p *Project) Validate() error {
	if !identifier.MatchString(p.ID) {
		return fmt.Errorf("프로젝트 ID는 영문, 숫자, 하이픈, 밑줄로 1~64자를 입력하세요")
	}
	p.Name = strings.TrimSpace(p.Name)
	if p.Name == "" || len([]rune(p.Name)) > 80 {
		return fmt.Errorf("프로젝트 이름은 1~80자를 입력하세요")
	}
	if len(p.Sites) == 0 || len(p.Sites) > 30 {
		return fmt.Errorf("사이트를 1~30개 등록하세요")
	}
	ids := map[string]bool{}
	for i := range p.Sites {
		s := &p.Sites[i]
		s.Name = strings.TrimSpace(s.Name)
		s.URL = strings.TrimSpace(s.URL)
		if !identifier.MatchString(s.ID) || ids[s.ID] {
			return fmt.Errorf("사이트 ID가 잘못되었거나 중복됩니다")
		}
		ids[s.ID] = true
		if s.Name == "" || len([]rune(s.Name)) > 80 {
			return fmt.Errorf("사이트 이름은 1~80자를 입력하세요")
		}
		u, e := url.Parse(s.URL)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.Fragment != "" || len(s.URL) > 2048 {
			return fmt.Errorf("사이트 URL은 계정 정보와 # 없이 http 또는 https 주소를 입력하세요")
		}
		if len(s.AllowedHosts) > 30 {
			return fmt.Errorf("허용 호스트는 30개 이하로 등록하세요")
		}
		hosts := []string{strings.ToLower(u.Hostname())}
		seen := map[string]bool{hosts[0]: true}
		for _, h := range s.AllowedHosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" {
				continue
			}
			parsed, e := url.Parse("https://" + h)
			if net.ParseIP(h) == nil && (e != nil || parsed.Hostname() != h || parsed.User != nil || parsed.Path != "" || strings.ContainsAny(h, "*?# ")) {
				return fmt.Errorf("허용 호스트에는 정확한 호스트 이름만 입력하세요")
			}
			if !seen[h] {
				hosts = append(hosts, h)
				seen[h] = true
			}
		}
		s.AllowedHosts = hosts
	}
	if !ids[p.DefaultSite] {
		return fmt.Errorf("기본 사이트를 선택하세요")
	}
	return nil
}
func Initial(cfg *config.Config) Registry {
	p := Project{ID: cfg.Project.ID, Name: cfg.Project.ID, DefaultSite: cfg.DefaultEnv().Name, Revision: 1}
	for _, id := range cfg.EnvNames() {
		e, _ := cfg.Env(id)
		p.Sites = append(p.Sites, Site{ID: id, Name: id, URL: e.BaseURL, AllowedHosts: e.AllowedHosts, ReadOnly: e.ReadOnly})
	}
	return Registry{DefaultProject: p.ID, Projects: []Project{p}}
}
func load(path string, initial Registry) (Registry, error) {
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return initial, save(path, initial)
	}
	if e != nil {
		return Registry{}, e
	}
	var r Registry
	if e = json.Unmarshal(b, &r); e != nil {
		return r, e
	}
	ids := map[string]bool{}
	for i := range r.Projects {
		p := &r.Projects[i]
		if e = p.Validate(); e != nil {
			return r, e
		}
		if ids[p.ID] {
			return r, fmt.Errorf("duplicate project")
		}
		ids[p.ID] = true
	}
	if !ids[r.DefaultProject] {
		return r, fmt.Errorf("default project unavailable")
	}
	return r, nil
}
func save(path string, r Registry) error {
	b, e := json.MarshalIndent(r, "", "  ")
	if e != nil {
		return e
	}
	if e = os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return e
	}
	f, e := os.CreateTemp(filepath.Dir(path), ".projects-*")
	if e != nil {
		return e
	}
	defer os.Remove(f.Name())
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	closeErr := f.Close()
	if e != nil {
		return e
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), path)
}

// Config preserves the original project's data and credentials. New projects
// get separate state, evidence and imports and no inherited credentials/sources.
func Config(base *config.Config, p Project, root string) *config.Config {
	c := *base
	if p.ID != base.Project.ID {
		c.Project.ID = p.ID
		c.Project.Repo = ""
		dir := filepath.Join(root, p.ID)
		c.State.Path = filepath.Join(dir, "state.db")
		c.Evidence.Dir = filepath.Join(dir, "evidence")
		c.Paths.Scenarios = filepath.Join(dir, "scenarios")
		c.Paths.Flows = filepath.Join(dir, "flows")
		c.Personas = nil
		c.Targets = nil
		c.Discovery.Adapters = []string{config.AdapterFile}
		c.Discovery.Adapter = config.AdapterFile
		c.Discovery.FeaturesDir = filepath.Join(dir, "features")
	}
	c.Target.URLFile = ""
	c.Target.DefaultEnv = p.DefaultSite
	c.Target.Environments = map[string]config.Environment{}
	for _, s := range p.Sites {
		c.Target.Environments[s.ID] = config.Environment{Name: s.ID, BaseURL: s.URL, AllowedHosts: append([]string(nil), s.AllowedHosts...), ReadOnly: s.ReadOnly}
		if s.ID == p.DefaultSite {
			c.Target.BaseURL = s.URL
			c.Target.AllowedHosts = append([]string(nil), s.AllowedHosts...)
		}
	}
	return &c
}
