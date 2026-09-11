# ADR-0003: Launched browsers are shut down when idle

## Status

Accepted

## Date

2026-09-07

## Context

The `loop` process keeps every browser it launches alive for its own lifetime. On the reference machine after four hours that was one `chrome-headless-shell` tree (about 400 MB across six processes) plus `lightpanda serve` (about 200 MB), resident even outside active hours when no cadence work runs. The Go processes themselves are small: the loop at about 56 MB, the dashboard at about 22 MB.

During a Browser Agent job the agent's own Chrome (launched by `agent-browser`) reaches about 1.9 GB across a dozen processes. That is Chrome's nature and already bounded: the agent closes its sessions when a job ends, and `AGENT_BROWSER_IDLE_TIMEOUT_MS` is set to five minutes as a backstop.

Relaunching a browser costs one to two seconds, which is negligible against a run that takes five to fifteen seconds and a cadence measured in minutes.

## Decision

- `browser.Provider` gains `Running()`, so a caller can tell a launched process from an attached one. Stopping an attached Lightpanda has always been a no-op and stays one.
- The runner counts runs holding a page per browser kind and records when the last one released it. `Runner.StopIdle(idle)` stops every launched browser with no run in flight and no run within `idle`, dropping its cached CDP connection so the next run reconnects to a fresh process.
- The scheduler calls this on every tick, including outside active hours, governed by `browser.idle_stop` (default 10m).
- Chromium is launched with a renderer process cap and without component updates, default apps, crash reporting, metrics upload, translation or media routing.

## Consequences

- Between runs, and for the whole of the inactive window, the loop holds only its own heap.
- The first run after an idle gap pays the relaunch. Set `idle_stop` to a large value (for example `24h`) to keep the previous behaviour.
- A run that fails to open a page still counts as use, so a browser that keeps failing is not churned by the idle sweep.
