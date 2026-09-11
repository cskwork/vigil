package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func loadEnvCfg(t *testing.T, targetYAML string) (*Config, error) {
	t.Helper()
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vigil.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 4\nproject:\n  id: p\ntarget:\n"+targetYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	return Load(cfgPath)
}

func TestImplicitDefaultEnvFromBaseURL(t *testing.T) {
	c, err := loadEnvCfg(t, "  base_url: https://www.example.com\n  allowed_hosts: [example.com]\n")
	if err != nil {
		t.Fatal(err)
	}
	if c.Target.DefaultEnv != ImplicitEnvName || len(c.EnvNames()) != 1 {
		t.Fatalf("default_env %q names %v", c.Target.DefaultEnv, c.EnvNames())
	}
	e := c.DefaultEnv()
	if e.Name != "default" || e.BaseURL != "https://www.example.com" || e.ReadOnly || len(e.AllowedHosts) != 1 || e.AllowedHosts[0] != "example.com" {
		t.Fatalf("implicit env %+v", e)
	}
	if got, err := c.Env(""); err != nil || got.Name != "default" {
		t.Fatalf("Env(\"\") = %+v, %v", got, err)
	}
	if got, err := c.Env("default"); err != nil || got.BaseURL != e.BaseURL {
		t.Fatalf("Env(default) = %+v, %v", got, err)
	}
	if _, err := c.Env("prod"); err == nil || !strings.Contains(err.Error(), "unknown environment") {
		t.Fatalf("unknown env must fail, got %v", err)
	}
	// a Config built without Load (tests, embedders) still resolves the implicit env
	bare := &Config{}
	bare.Target.BaseURL = "https://h.test"
	bare.Target.AllowedHosts = []string{"h.test"}
	if got, err := bare.Env(""); err != nil || got.Name != "default" || got.BaseURL != "https://h.test" {
		t.Fatalf("bare Env = %+v, %v", got, err)
	}
}

const multiEnvYAML = `  default_env: stg
  environments:
    stg:
      base_url: https://staging.example.com
      allowed_hosts: [staging.example.com]
      asset_page: https://staging.example.com/app/training-entry
    prod:
      base_url: https://www.example.com
      read_only: true
`

func TestEnvironmentsResolveAndDefaultTarget(t *testing.T) {
	c, err := loadEnvCfg(t, multiEnvYAML)
	if err != nil {
		t.Fatal(err)
	}
	if got := c.EnvNames(); strings.Join(got, ",") != "prod,stg" {
		t.Fatalf("names %v", got)
	}
	stg := c.DefaultEnv()
	if stg.Name != "stg" || stg.BaseURL != "https://staging.example.com" || stg.ReadOnly || stg.AssetPage == "" {
		t.Fatalf("default env %+v", stg)
	}
	prod, err := c.Env("prod")
	if err != nil {
		t.Fatal(err)
	}
	// allowed_hosts derived from base_url; read_only may omit asset_page
	if !prod.ReadOnly || prod.AssetPage != "" || len(prod.AllowedHosts) != 1 || prod.AllowedHosts[0] != "www.example.com" {
		t.Fatalf("prod env %+v", prod)
	}
	if !prod.URLAllowed("https://www.example.com/x") || prod.URLAllowed("https://staging.example.com/x") || !stg.HostAllowed("api.staging.example.com") {
		t.Fatal("allowlist checks")
	}
	// the default environment becomes the primary target for legacy readers
	if c.Target.BaseURL != "https://staging.example.com" || !c.HostAllowed("staging.example.com") {
		t.Fatalf("target %q hosts %v", c.Target.BaseURL, c.Target.AllowedHosts)
	}
}

func TestEnvironmentValidation(t *testing.T) {
	cases := []struct{ name, yaml, want string }{
		{"unknown default_env", "  default_env: qa\n  environments:\n    stg:\n      base_url: https://a.test\n", `target.default_env "qa" is unknown`},
		{"unknown default_env without environments", "  base_url: https://a.test\n  default_env: qa\n", `target.default_env "qa" is unknown`},
		{"missing base_url", "  environments:\n    stg:\n      allowed_hosts: [a.test]\n", "target.environments.stg.base_url is required"},
		{"relative base_url", "  environments:\n    stg:\n      base_url: /x\n", "must be an absolute http(s) URL"},
		{"default_env required with several envs", "  environments:\n    a:\n      base_url: https://a.test\n    b:\n      base_url: https://b.test\n", "target.default_env is required"},
		{"asset_page outside allowlist", "  environments:\n    a:\n      base_url: https://a.test\n      asset_page: https://b.test/p\n", "asset_page"},
	}
	for _, tc := range cases {
		_, err := loadEnvCfg(t, tc.yaml)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// a single environment is the default without default_env
	c, err := loadEnvCfg(t, "  environments:\n    only:\n      base_url: https://a.test\n")
	if err != nil || c.Target.DefaultEnv != "only" {
		t.Fatalf("single env default: %v %q", err, c.Target.DefaultEnv)
	}
}

func TestEnvAllows(t *testing.T) {
	rw := Environment{Name: "stg"}
	ro := Environment{Name: "prod", ReadOnly: true}
	for _, m := range []string{"", "read-only", "reversible", "destructive"} {
		if err := EnvAllows(rw, m); err != nil {
			t.Errorf("rw %q: %v", m, err)
		}
	}
	for _, m := range []string{"", "read-only"} {
		if err := EnvAllows(ro, m); err != nil {
			t.Errorf("ro %q: %v", m, err)
		}
	}
	for _, m := range []string{"reversible", "destructive"} {
		err := EnvAllows(ro, m)
		if err == nil || !strings.Contains(err.Error(), `environment "prod" is read-only`) || !strings.Contains(err.Error(), m) {
			t.Errorf("ro %q: %v", m, err)
		}
	}
}
