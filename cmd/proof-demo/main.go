// proof-demo is an owned synthetic deployed target. Its only writes affect QA fixtures.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	_ "github.com/go-sql-driver/mysql"
	"net/http"
	"os"
	"sync"
	"time"
	"vigil/internal/dsl"
	"vigil/internal/proof"
)

type app struct {
	mu         sync.Mutex
	value      string
	generation string
	version    string
	db         *sql.DB
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8790", "synthetic target")
	version := flag.String("version", "C", "A=stale API, B=history loss, C=correct")
	registry := flag.String("registry", "", "write registry JSON")
	registryOnly := flag.Bool("registry-only", false, "write registry and exit without starting a target")
	dsnEnv := flag.String("mysql-env", "PROOF_DEMO_MYSQL_DSN", "optional fixture DB environment reference")
	flag.Parse()
	if *registryOnly {
		if *registry == "" {
			panic("--registry is required")
		}
		b, _ := json.MarshalIndent(makeRegistry("http://"+*addr), "", "  ")
		if e := os.WriteFile(*registry, b, 0600); e != nil {
			panic(e)
		}
		return
	}
	a := &app{value: "0", version: *version}
	if d := os.Getenv(*dsnEnv); d != "" {
		var e error
		a.db, e = sql.Open("mysql", d)
		if e != nil {
			panic(e)
		}
		a.db.SetMaxOpenConns(1)
		if _, e = a.db.Exec(`CREATE TABLE IF NOT EXISTS proof_history(entity VARCHAR(100),id INT,value VARCHAR(100),PRIMARY KEY(entity,id))`); e != nil {
			panic(e)
		}
	}
	origin := "http://" + *addr
	if *registry != "" {
		b, _ := json.MarshalIndent(makeRegistry(origin), "", "  ")
		if e := os.WriteFile(*registry, b, 0600); e != nil {
			panic(e)
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["entity"] != "qa-record" || in["attempt"] == "" {
			http.Error(w, "bad fixture", 400)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.value = "0"
		a.generation = in["attempt"]
		if a.db != nil {
			if _, e := a.db.ExecContext(r.Context(), `DELETE FROM proof_history WHERE entity='qa-record'`); e != nil {
				http.Error(w, "DB unavailable", 503)
				return
			}
			if _, e := a.db.ExecContext(r.Context(), `INSERT INTO proof_history VALUES('qa-record',1,'keep'),('qa-record',2,'preserved')`); e != nil {
				http.Error(w, "DB unavailable", 503)
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"entity": in["entity"], "attempt": a.generation, "ready": true})
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><html lang="ko"><head><meta charset="utf-8"><title>QA fixture</title></head><body><h1>QA 기록</h1><p id="build">%s</p><label>값 <input id="value" value="0"></label><button id="save">저장</button><p id="status">준비</p><output id="actual" style="display:block;min-height:2rem;min-width:8rem;border:1px solid #aaa">0</output><script src="/app.js"></script></body></html>`, a.version)
	})
	mux.HandleFunc("GET /app.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprint(w, `document.querySelector('#save').onclick=async()=>{const value=document.querySelector('#value').value;const saved=await fetch('/record',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({entity:'qa-record',value})});const data=await saved.json();document.querySelector('#status').textContent=data.success?'저장 완료':'실패';const reread=await (await fetch('/record?entity=qa-record')).json();document.querySelector('#actual').textContent=value;};`)
	})
	mux.HandleFunc("POST /record", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]string
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["entity"] != "qa-record" {
			http.Error(w, "bad fixture", 400)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		a.value = in["value"]
		if a.db != nil && a.version == "B" {
			if _, e := a.db.ExecContext(r.Context(), `DELETE FROM proof_history WHERE entity='qa-record' AND id=1`); e != nil {
				http.Error(w, "DB failed", 503)
				return
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "entity": "qa-record", "generation": a.generation})
	})
	mux.HandleFunc("GET /record", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("entity") != "qa-record" {
			http.Error(w, "entity required", 400)
			return
		}
		a.mu.Lock()
		defer a.mu.Unlock()
		v := a.value
		if a.version == "A" {
			v = "0"
		}
		json.NewEncoder(w).Encode(map[string]any{"success": true, "entity": "qa-record", "value": v, "generation": a.generation})
	})
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	server := &http.Server{Addr: *addr, Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	fmt.Println("ProofQA synthetic target:", origin, "version", *version)
	if e := server.ListenAndServe(); e != nil {
		panic(e)
	}
}
func makeRegistry(origin string) proof.Registry {
	scope := []proof.ScopeRule{}
	for _, p := range []string{"/", "/app.js", "/favicon.ico"} {
		scope = append(scope, proof.ScopeRule{Origin: origin, Method: "GET", Path: p})
	}
	scope = append(scope, proof.ScopeRule{Origin: origin, Method: "POST", Path: "/reset"}, proof.ScopeRule{Origin: origin, Method: "POST", Path: "/record"}, proof.ScopeRule{Origin: origin, Method: "GET", Path: "/record", Query: map[string]string{"entity": "qa-record"}})
	con := proof.Contract{Persona: "tester", Fixture: "record", Actions: []string{"save"}, Criteria: []proof.Criterion{{ID: "ui", Title: "화면 값이 빈 문자열입니다", Required: true, Observer: "ui", Expected: proof.Value{Present: true, Data: json.RawMessage(`""`)}, Source: "definition", SourceRef: "empty"}, {ID: "api", Title: "다시 조회한 API 값이 빈 문자열입니다", Required: true, Observer: "api", Expected: proof.Value{Present: true, Data: json.RawMessage(`""`)}, Source: "definition", SourceRef: "empty"}, {ID: "history", Title: "저장 전후 이력의 키와 값이 보존됩니다", Required: true, Observer: "history", Expected: proof.Value{Present: true, Data: json.RawMessage(`true`)}, Source: "definition", SourceRef: "preserved"}}}
	t := proof.Target{Title: "독립 QA 데모", BaseURL: origin, Environment: "qa", PolicyVersion: "demo-v1", VersionSelector: "#build", Template: &con, Scope: scope, Personas: map[string]proof.Persona{"tester": {Account: "proof-demo-tester"}}, Fixtures: map[string]proof.Fixture{"record": {Entity: "qa-record", QA: true, PrepareURL: origin + "/reset"}}, Actions: map[string]proof.Action{"save": {Title: "QA 기록을 빈 값으로 저장하고 다시 조회", Mutating: true, Steps: []dsl.Step{{Goto: origin + "/"}, {Fill: &dsl.FillArgs{Locator: dsl.Locator{By: "css", Value: "#value"}, Input: ""}}, {Click: &dsl.Locator{By: "css", Value: "#save"}}, {WaitFor: &dsl.Locator{By: "text", Text: "저장 완료"}}}}}, Observers: map[string]proof.Observer{"ui": {Kind: "dom", Action: "save", Selector: "#actual", Property: "text"}, "api": {Kind: "network", Action: "save", API: &scope[len(scope)-1], JSONPath: "$.value", SuccessPath: "$.success", Success: proof.Value{Present: true, Data: json.RawMessage(`true`)}, Reread: true, WriteAPI: &scope[len(scope)-2], EntityPath: "$.entity", GenerationPath: "$.generation"}, "history": {Kind: "mysql", Action: "save", Probe: "history"}}, Definitions: map[string]proof.Definition{"empty": {Title: "빈 문자열", Expected: proof.Value{Present: true, Data: json.RawMessage(`""`)}}, "preserved": {Title: "이력 보존", Expected: proof.Value{Present: true, Data: json.RawMessage(`true`)}}}}
	return proof.Registry{Targets: map[string]proof.Target{"demo": t}, Probes: map[string]proof.Probe{"history": {DSNEnv: "PROOF_MYSQL_DSN", Query: "SELECT id,value FROM proof_history WHERE entity=? ORDER BY id", Keys: []string{"id"}, Fields: []string{"value"}}}}
}
