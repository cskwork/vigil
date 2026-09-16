package main

import (
	"context"
	"flag"
	"os"

	"vigil/internal/browsermcp"
)

// cmdMCPBrowser serves the bounded agent_browser tool over MCP stdio. A coding
// agent the operator already pays for (Claude Code, Codex) is pointed at this
// command, so it drives the same browser under the same restrictions as vigil's
// own pi extension. It needs no config and no store.
func (a *app) cmdMCPBrowser(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp-browser", flag.ContinueOnError)
	session := fs.String("session", os.Getenv("AGENT_BROWSER_SESSION"), "base agent-browser session name")
	timeout := fs.Duration("timeout", browsermcp.DefaultTimeout, "per-command timeout")
	maxOutput := fs.Int("max-output", browsermcp.DefaultMaxOutput, "maximum characters returned per command")
	quiet := fs.Bool("quiet", false, "do not write diagnostics to stderr")
	if err := fs.Parse(args); err != nil {
		return err
	}
	srv := &browsermcp.Server{Session: *session, Timeout: *timeout, MaxOutput: *maxOutput, Log: os.Stderr}
	if *quiet {
		srv.Log = nil
	}
	if srv.Timeout <= 0 {
		srv.Timeout = browsermcp.DefaultTimeout
	}

	return srv.Serve(ctx, os.Stdin, os.Stdout)
}
