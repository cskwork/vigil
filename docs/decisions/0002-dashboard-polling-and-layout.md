# ADR-0002: Dashboard polls with content ETags, and the ui package is split by endpoint

## Status

Accepted

## Date

2026-09-07

## Context

The dashboard pages poll `/api/overview` every 3 s and `/api/verification` every 5 s from every open tab. Against a real deployment (78 scenarios, 1,180 runs, 180 agent transcripts) one overview response took about 100 ms and 150 KB, and each page rebuilt its whole DOM on every poll, which closed any open `<details>` and dropped text selections every few seconds.

Profiling showed the cost was not the database. `ActiveHours.NextChange` scanned minute by minute for up to eight days and called `time.LoadLocation` on every step, so a single window computation read and parsed the zoneinfo file more than 11,000 times. The rest was an N+1 pattern (three queries per scenario per poll), re-parsing every run's `network.json` for the verification table, and walking the entire `evidence/agent` tree on every request.

`internal/ui/ui.go` had grown to a thousand lines covering seven endpoints, and the four HTML pages each carried their own copy of the Korean vocabulary for machine states, which had already started to drift between pages.

## Decision

1. **Zone lookups are memoized and `NextChange` walks window boundaries.** `Allows` can only flip at a `from` or `to` clock time on some day, so the next change is the earliest such boundary after now that flips the result. A test asserts this against the original minute scan for same-day, overnight, weekend-only and malformed windows.
2. **Polled endpoints carry a content ETag computed before the wall clock is stamped in.** Responses are `Cache-Control: no-cache`; the browser revalidates and receives 304 when nothing changed. The page compares ETags and skips its re-render on a repeat, keeping open panels intact. A separate 30 s tick refreshes relative times.
3. **Batch store reads.** `ListScenarioMetrics`, `ListCurrentScenarioVersions`, `ListScenarioVersionHistory` (no YAML bodies) and `ListAllCoverageLinks` replace the per-scenario loops. The scripts page ships only the current version's YAML; older bodies load on demand and are cached hard, since a version row never changes.
4. **Filesystem-derived views are cached against `(size, mtime)`.** A run directory is write-once, so its evidence summary is keyed on `steps.json`; an agent transcript only grows, so its parsed shape is keyed the same way; the agent tree walk runs at most once every 2 s.
5. **`internal/ui` is one file per endpoint** (`server.go`, `overview.go`, `verification.go`, `evidence.go`, `agent.go`, `scripts.go`, `requests_list.go`, `format.go`), and the pages share `app.js` for vocabulary, time formatting, the poller, keyboard-reachable rows and `<details>` state.
6. **Mutating routes require same origin.** The schedule window handler now applies the same `Origin`/`Sec-Fetch-Site` check the request form already had; read endpoints answer 405 to writes.

## Consequences

- Overview response time on the reference deployment dropped from about 90 ms to about 6 ms; a repeat poll transfers roughly 300 bytes instead of 150 KB.
- Static pages, `theme.css` and `app.js` carry an ETag from their embedded content, so an upgraded binary is picked up immediately without hard refresh.
- The dashboard follows the system colour scheme, with an explicit light/dark override in the rail. Status never depends on colour alone; the token pairs keep at least 4.5:1 in both schemes.
- The ETag ignores `now`; any field that changes on every request must likewise be added after hashing, or polling will never see a 304.
- Timestamps are still rendered in the server's local zone. The dashboard is an intranet tool that sits in the same zone as the loop; a cross-zone deployment would need the API to emit offsets.

## Alternatives considered

- **Server-sent events instead of polling.** Fewer requests, but the pages already worked on plain fetches and a 304 costs almost nothing once the server no longer recomputes the window. Revisit if the number of open tabs grows.
- **Diffing the DOM instead of skipping renders.** Would also preserve state, but needs a templating layer the pages deliberately avoid.
