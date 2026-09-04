# Spec: User-prioritized QA

## Objective

A non-developer can describe a QA situation in one main-page text box. Vigil registers it as a bounded read-only Browser Agent request, safely yields the currently running Browser Agent job, and runs the new request next. The displaced job returns to the queue. Deterministic scenario workers continue uninterrupted.

## Tech Stack

Go HTTP server, embedded HTML/CSS/JavaScript, SQLite job queue, existing Browser Agent and candidate validation pipeline.

## Commands

- Test: `go test ./...`
- Focused test: `go test ./internal/ui ./internal/orchestrator ./internal/scheduler ./internal/store`
- Static analysis: `go vet ./...`
- Build: `go build ./cmd/vigil`
- Runtime: `./vigil loop --ui 127.0.0.1:8787`

## Project Structure

- `internal/orchestrator/` — shared manual-request validation and registration
- `internal/scheduler/` — Browser Agent worker preemption and safe requeue
- `internal/ui/` — HTTP intake, current state, and main-page form
- `cmd/vigil/` — CLI reuse of the shared request contract and runtime wiring
- `docs/decisions/` — accepted design rationale

## Code Style

```go
result, err := submitter.SubmitUserRequest(r.Context(), input)
if err != nil {
    http.Error(w, err.Error(), http.StatusBadRequest)
    return
}
writeJSON(w, http.StatusCreated, result)
```

Use small concrete interfaces at package boundaries. Validate HTTP input once. Keep existing queue and scenario lifecycle types.

## Testing Strategy

- Unit-test request defaults, limits, unique IDs, payload, and priority.
- Unit-test safe cancellation and requeue of only an Agent job.
- HTTP-test valid, empty, malformed, and oversized submissions plus method handling.
- Run all Go tests and static analysis.
- Start an isolated runtime with temporary state, submit through the HTML/API, and verify the visible state transition.

## Boundaries

- Always: trim and bound input; force `read-only`; use server-generated identifiers; preserve candidate validation and SOAK.
- Ask first: authentication, remote/public binding policy, mutable requests, new database schema.
- Never: expose account, mutation, timeout, or tool-budget controls in this first UI; interrupt deterministic scenario workers; discard the displaced Agent job.

## Success Criteria

1. The main page always shows one clearly labelled QA-situation textarea and submit button.
2. A valid submission creates a manual feature and `AGENT_DISCOVER` job with user priority above repair priority.
3. If an Agent job is running in the same loop process, it is cancelled cooperatively and returned to READY without consuming an attempt or Agent budget.
4. The user job runs next, subject to Browser Agent availability. User-initiated preemption is allowed to bypass the hourly Agent budget once so “run now” is truthful.
5. The displaced job resumes later. Deterministic jobs are never interrupted.
6. The page shows queued, preempting, running, budget-waiting, done, or failed from persisted job state.
7. Empty, malformed, oversized, cross-origin, and non-POST mutations are rejected.
8. Existing CLI manual requests use the same validation and persistence contract.

## Assumptions

- The dashboard remains a trusted local/operator surface; this feature does not make `0.0.0.0` safe for untrusted networks.
- “Interrupt” means cooperative context cancellation and safe requeue, not killing the whole Vigil process.
- User prose is evidence and exploration guidance, not an approved oracle.

## Open Questions

None for this slice.
