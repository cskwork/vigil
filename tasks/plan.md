# Plan: User-prioritized QA

Task type · feature

Goal · one-box main-page QA intake that preempts and safely requeues the active Browser Agent job

Files · `internal/orchestrator`, `internal/scheduler`, `internal/ui`, `cmd/vigil`, model constants, tests, README/PRD, ADR

Contracts · read-only input; server-generated ID; priority above repairs; cooperative Agent-only preemption; existing validation gate and SOAK preserved

Verification · focused Go tests, full `go test ./...`, `go vet ./...`, build, isolated HTTP/browser submission

Assumptions · trusted dashboard; no deterministic-worker preemption; no schema migration

Adversarial revision · a DB-only lease reset can run the old and new jobs concurrently. The implementation must cancel the in-process Agent context, wait for it to return, and only then let the worker claim the higher-priority request while requeuing the displaced job.
