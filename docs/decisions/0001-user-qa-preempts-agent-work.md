# ADR-0001: User QA requests preempt Browser Agent work

## Status

Accepted

## Date

2026-09-03

## Context

Operators can already submit manual QA through a CLI file, but the main dashboard only displays results. Manual requests use priority 90, below repair work at 100, and cannot interrupt a long Browser Agent task. The requested experience is one plain-language box that starts the operator's QA immediately.

The dashboard has no authentication and may be bound beyond loopback. Browser Agent requests can navigate a deployed site, so accepting arbitrary mutation settings or URLs would expand authority dangerously.

## Decision

Add a one-field, read-only dashboard intake. The server generates identifiers and execution limits. User requests receive a dedicated priority above repairs.

When the dashboard is hosted by the loop process, submitting a request cooperatively cancels only the current Browser Agent context. Vigil returns that job to READY before the Agent worker claims the new request. Priority 110 is persisted as a one-shot budget entitlement and atomically drops to normal direct-coverage priority on its first claim, so restarts preserve it but retries cannot reuse it. Functional scenario workers are unaffected. The existing candidate validation and SOAK lifecycle remains mandatory.

## Alternatives Considered

### Queue without preemption

Simpler, but a long exploration can delay an explicit operator request by many minutes and violates the confirmed “interrupt current work” behavior.

### Expose the complete CLI request schema

More flexible, but it exposes mutation, accounts, URLs, and execution budgets on an unauthenticated surface. Existing Agent jobs also do not acquire request locks before exploration.

### Let the coverage supervisor interpret feedback

Keeps the dashboard write surface small, but execution waits for the supervisor tick and does not guarantee next-job priority.

## Consequences

- Operators get an immediate, low-friction intake and visible receipt.
- A cancelled Agent task may repeat some browser work when resumed, but is not lost.
- User submission can spend one Agent task outside the cadence budget; this is explicit operator work, not autonomous growth.
- Remote dashboard exposure remains unsafe without a later authentication and CSRF design.
