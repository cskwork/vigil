package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"vigil/internal/admin"
	"vigil/internal/approval"
	"vigil/internal/config"
	"vigil/internal/evidence"
	"vigil/internal/model"
	"vigil/internal/orchestrator"
	"vigil/internal/scheduler"
	"vigil/internal/store"
	"vigil/internal/ui"
)

type adminActions struct {
	ui.StoreScriptActions
	ctx      context.Context
	sched    *scheduler.Scheduler
	busy     atomic.Bool
	wg       sync.WaitGroup
	logError func(error)
	progress *executionTracker
	stopIdle func()
}

func (a *adminActions) CanRun() bool { return true }
func (a *adminActions) RunScript(ctx context.Context, sc *model.Scenario, env config.Environment, b model.Browser) (int64, error) {
	if !a.busy.CompareAndSwap(false, true) {
		return 0, fmt.Errorf("다른 검사가 실행 중입니다. 완료 후 다시 실행하세요")
	}
	id, _, e := a.sched.EnqueueScenarioEnv(ctx, sc, model.PriorityUserRequest, b, "", env.Name)
	if e != nil {
		a.busy.Store(false)
		return 0, e
	}
	a.progress.begin(id, sc, env.Name, env.BaseURL)
	a.wg.Add(1)
	go func() {
		defer a.wg.Done()
		defer a.busy.Store(false)
		err := a.sched.RunJobNow(a.ctx, id)
		a.progress.complete(id, err)
		if err != nil {
			a.logError(err)
		}
		if a.stopIdle != nil {
			a.stopIdle()
		}
	}()
	return id, nil
}
func (a *app) cmdAdmin(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("admin", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "local admin address")
	registry := fs.String("registry", filepath.Join(a.stateDir(), "admin-projects.json"), "project registry")
	headless := fs.Bool("headless", false, "run browser without a visible window")
	dev := fs.String("dev-ui", "", "UI asset directory")
	if e := fs.Parse(args); e != nil {
		return e
	}
	host, _, e := net.SplitHostPort(*addr)
	if e != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("admin은 로컬 loopback 주소에서만 실행할 수 있습니다")
	}
	registryPath, e := filepath.Abs(*registry)
	if e != nil {
		return e
	}
	root := filepath.Join(filepath.Dir(registryPath), "projects")
	console, e := admin.New(registryPath, admin.Initial(a.cfg), func(p admin.Project) (admin.Runtime, error) {
		cfg := admin.Config(a.cfg, p, root)
		cfg.Browser.Chromium.Headless = *headless
		st, e := store.Open(cfg.Abs(cfg.State.Path))
		if e != nil {
			return admin.Runtime{}, e
		}
		if e = st.UpsertProject(ctx, p.ID, cfg.Target.BaseURL); e != nil {
			st.Close()
			return admin.Runtime{}, e
		}
		child := &app{cfg: cfg, st: st, ev: evidence.New(cfg.Abs(cfg.Evidence.Dir)), log: a.log, out: a.out}
		run := child.buildRunner()
		orch := child.buildOrchestrator(run, nil)
		progress := &executionTracker{st: st, project: p.ID, root: cfg.Abs(cfg.Evidence.Dir)}
		sched := scheduler.NewWith(cfg, st, orch, progressRunner{run: run, tracker: progress}, nil, nil)
		sched.Log = a.log
		runCtx, cancel := context.WithCancel(ctx)
		actions := &adminActions{StoreScriptActions: ui.StoreScriptActions{Svc: &approval.Service{Cfg: cfg, St: st}}, ctx: runCtx, sched: sched, progress: progress, stopIdle: func() { run.StopIdle(0) }, logError: func(e error) { a.log.Printf("admin run: %v", e) }}
		server := ui.New(cfg, st, cfg.Abs(cfg.Evidence.Dir))
		server.SetScriptActions(actions)
		server.SetRequestSubmitter(&adminRequests{app: child, actions: actions, run: progressRunner{run: run, tracker: progress}})
		server.SetOnDemandMode()
		if *dev != "" {
			server.SetDevDir(*dev)
		}
		handler := server.Handler()
		mux := http.NewServeMux()
		mux.Handle("/", handler)
		mux.HandleFunc("/api/script/import", draftHandler(orch))
		mux.HandleFunc("/api/execution", progress.handler)
		return admin.Runtime{Handler: mux, Busy: actions.busy.Load, Close: func() { cancel(); actions.wg.Wait(); child.close() }}, nil
	})
	if e != nil {
		return e
	}
	defer console.Close()
	a.printf("Vigil admin: http://%s/\n", *addr)
	// DNS rebinding protection: a remote Host must not access the local console.
	return admin.Listen(ctx, *addr, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h, _, e := net.SplitHostPort(r.Host)
		if e != nil {
			h = r.Host
		}
		ip := net.ParseIP(h)
		if h != "localhost" && (ip == nil || !ip.IsLoopback()) {
			http.Error(w, "local host required", 403)
			return
		}
		console.ServeHTTP(w, r)
	}))
}
func draftHandler(o *orchestrator.Orchestrator) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, s string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": s})
		}
		if r.Method != "POST" {
			fail(405, "POST 요청이 필요합니다")
			return
		}
		if !ui.SameOrigin(r) {
			fail(403, "다른 출처에서는 검사를 등록할 수 없습니다")
			return
		}
		if r.Header.Get("Content-Type") != "application/json" {
			fail(415, "JSON 형식이 필요합니다")
			return
		}
		var in struct {
			YAML string `json:"yaml"`
		}
		dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 256<<10))
		dec.DisallowUnknownFields()
		if dec.Decode(&in) != nil || dec.Decode(&struct{}{}) != io.EOF {
			fail(400, "검사 내용 형식이 올바르지 않습니다")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		defer cancel()
		id, e := o.ImportDraft(ctx, []byte(in.YAML))
		if e != nil {
			fail(422, "등록하지 못했습니다: "+e.Error())
			return
		}
		w.WriteHeader(201)
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	}
}
