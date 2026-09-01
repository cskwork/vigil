package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestURLFileDerivesTarget(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "url.md"), []byte("# targets\n\ntraining-entry | https://www.example.com/lms-web/training-entry?x=1\nhttps://www.example.com/other\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "vigil.yaml")
	if err := os.WriteFile(cfgPath, []byte("version: 4\nproject:\n  id: p\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if c.Target.BaseURL != "https://www.example.com" {
		t.Fatalf("base_url %q", c.Target.BaseURL)
	}
	if !c.HostAllowed("www.example.com") {
		t.Fatalf("host not allowlisted: %v", c.Target.AllowedHosts)
	}
	if len(c.Targets) != 2 || c.Targets[0].Label != "training-entry" || c.Targets[0].Path != "/lms-web/training-entry?x=1" || c.Targets[1].Label != "other" {
		t.Fatalf("targets %+v", c.Targets)
	}
	if c.EntryPath() != "/lms-web/training-entry?x=1" || len(c.TargetURLs()) != 2 {
		t.Fatalf("entry %q urls %v", c.EntryPath(), c.TargetURLs())
	}
}

func TestURLFileMissingKeepsValidation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "vigil.yaml")
	_ = os.WriteFile(cfgPath, []byte("version: 4\nproject:\n  id: p\n"), 0o644)
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("base_url must still be required without url.md")
	}
	_ = os.WriteFile(filepath.Join(dir, "url.md"), []byte("not a url\n"), 0o644)
	if _, err := Load(cfgPath); err == nil {
		t.Fatal("invalid url line must fail")
	}
}
