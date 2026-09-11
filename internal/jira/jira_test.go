package jira

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fakeClient(t *testing.T, loseWrite bool) (*Client, string) {
	t.Helper()
	cli, err := filepath.Abs("testdata/fake-acli.sh")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	t.Setenv("FAKE_ACLI_STORE", filepath.Join(dir, "store.txt"))
	t.Setenv("FAKE_ACLI_LOG", filepath.Join(dir, "argv.log"))
	if loseWrite {
		t.Setenv("FAKE_ACLI_LOSE_WRITE", "1")
	}
	return &Client{CLI: cli}, filepath.Join(dir, "argv.log")
}

const body = "[vigil] QA 스크립트 승인: entry-tabs v2, 탭 중복\n· 재현/회귀 실행: vigil run entry-tabs\n· 증거: evidence/runs/entry-tabs"

func TestCommentCreatesAndVerifies(t *testing.T) {
	c, logPath := fakeClient(t, false)
	id, err := c.Comment(context.Background(), "PROJ-123", body)
	if err != nil {
		t.Fatal(err)
	}
	if id != "10001" {
		t.Fatalf("id = %q", id)
	}
	argv, _ := os.ReadFile(logPath)
	got := string(argv)
	for _, want := range []string{"jira\nworkitem\ncomment\ncreate\n--key\nPROJ-123\n--body\n" + body + "\n--json\n", "jira\nworkitem\ncomment\nlist\n--key\nPROJ-123\n--json\n"} {
		if !strings.Contains(got, want) {
			t.Fatalf("argv log missing %q:\n%s", want, got)
		}
	}
}

func TestCommentNotVisibleAfterCreate(t *testing.T) {
	c, _ := fakeClient(t, true)
	_, err := c.Comment(context.Background(), "PROJ-123", body)
	if !errors.Is(err, ErrNotVisible) {
		t.Fatalf("err = %v, want ErrNotVisible", err)
	}
}

func TestCommentCLIFailure(t *testing.T) {
	c := &Client{CLI: filepath.Join(t.TempDir(), "missing-acli")}
	if _, err := c.Comment(context.Background(), "PROJ-123", body); err == nil || errors.Is(err, ErrNotVisible) {
		t.Fatalf("err = %v", err)
	}
}

func TestCommentDryRunWritesFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "jira")
	c := &Client{CLI: "definitely-not-on-path", DryRunDir: dir, Now: func() time.Time { return time.Date(2026, 9, 10, 1, 2, 3, 0, time.UTC) }}
	id, err := c.Comment(context.Background(), "PROJ-128", body)
	if err != nil {
		t.Fatal(err)
	}
	if !IsDryRun(id) || DryRunPath(id) != filepath.Join(dir, "PROJ-128-20260910T010203Z.md") {
		t.Fatalf("id = %q", id)
	}
	b, err := os.ReadFile(DryRunPath(id))
	if err != nil || strings.TrimSpace(string(b)) != body {
		t.Fatalf("file = %q, %v", b, err)
	}
}
