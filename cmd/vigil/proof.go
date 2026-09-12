package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"time"
	"vigil/internal/proof"
	"vigil/internal/store"
)

func (a *app) cmdProof(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("proof", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8788", "listen address")
	registry := fs.String("registry", "proof-registry.json", "registered targets")
	dbpath := fs.String("db", ".vigil/proof.db", "SQLite state")
	dir := fs.String("evidence", ".vigil/proof-evidence", "evidence directory")
	rawDays := fs.Int("raw-retention-days", 7, "raw evidence retention")
	summaryDays := fs.Int("summary-retention-days", 30, "summary retention")
	local := fs.Bool("local-operator", false, "explicit loopback operator mode")
	gatewayEnv := fs.String("gateway-token-env", "", "environment variable containing gateway bearer token")
	actor := fs.String("gateway-actor", "", "authenticated fixed gateway principal")
	usersFile := fs.String("users", "", "operator-provisioned login users JSON file")
	publicOrigin := fs.String("public-origin", "", "exact HTTPS origin for cookie login")
	if e := fs.Parse(args); e != nil {
		return e
	}
	host, _, e := net.SplitHostPort(*addr)
	if e != nil {
		return e
	}
	if *local && (net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback()) {
		return fmt.Errorf("local operator requires literal loopback bind")
	}
	b, e := os.ReadFile(*registry)
	if e != nil {
		return e
	}
	var reg proof.Registry
	if e = json.Unmarshal(b, &reg); e != nil {
		return e
	}
	if e = reg.Validate(); e != nil {
		return e
	}
	st, e := store.Open(*dbpath)
	if e != nil {
		return e
	}
	defer st.Close()
	abs, e := filepath.Abs(*dir)
	if e != nil {
		return e
	}
	svc, e := proof.NewService(st, reg, abs)
	if e != nil {
		return e
	}
	defer svc.Close()
	if *rawDays < 1 || *summaryDays < *rawDays {
		return fmt.Errorf("invalid retention days")
	}
	svc.RawRetention = time.Duration(*rawDays) * 24 * time.Hour
	svc.SummaryRetention = time.Duration(*summaryDays) * 24 * time.Hour
	if e = svc.Prune(ctx); e != nil {
		return e
	}
	var users []proof.LoginUser
	if *usersFile != "" {
		data, err := os.ReadFile(*usersFile)
		if err != nil {
			return err
		}
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err = decoder.Decode(&users); err != nil {
			return err
		}
		var tail any
		if decoder.Decode(&tail) != io.EOF {
			return fmt.Errorf("users file must contain one JSON value")
		}
	}
	h, e := svc.Handler(proof.HTTPConfig{Users: users, PublicOrigin: *publicOrigin, LocalOperator: *local, GatewayToken: os.Getenv(*gatewayEnv), GatewayActor: *actor, UI: proof.UI()})
	if e != nil {
		return e
	}
	listener, e := net.Listen("tcp", *addr)
	if e != nil {
		return e
	}
	server := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	workctx, cancel := context.WithCancel(ctx)
	defer cancel()
	errs := make(chan error, 2)
	workerDone := make(chan struct{})
	go func() { defer close(workerDone); errs <- svc.Run(workctx) }()
	go func() { errs <- server.Serve(listener) }()
	fmt.Fprintf(a.out, "ProofQA: http://%s\n", listener.Addr())
	select {
	case <-ctx.Done():
	case e = <-errs:
	}
	cancel()
	shut, stop := context.WithTimeout(context.Background(), 30*time.Second)
	defer stop()
	_ = server.Shutdown(shut)
	select {
	case <-workerDone:
	case <-shut.Done():
		return fmt.Errorf("proof worker teardown exceeded 30 seconds")
	}
	if ctx.Err() != nil {
		return nil
	}
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}
