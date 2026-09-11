# ADR-0005: Named environments and the read-only production rule

## Status

Accepted

## Date

2026-09-10

## Context

One deployment per project was enough while vigil only watched a staging target. Reproducing a reported issue changes that: the report usually comes from production, and the first question after a fix is "does it still happen on prod?". Answering it by editing `target.base_url` swaps the whole config, including the target the loop keeps polling.

Production is also where an accidental mutation costs the most. Our scenarios already declare `mutation: read-only | reversible | destructive`, and only the first is safe to point at real users' data.

## Decision

- `target.environments.<name>` carries `base_url`, `allowed_hosts`, an optional `asset_page` and `read_only`. `target.default_env` names the one everything uses unless a run says otherwise. An empty block synthesises one implicit environment called `default` from `target.base_url` / `allowed_hosts`, so existing configs behave exactly as before.
- `vigil run ... --env <name>` and the dashboard's environment select resolve relative `goto:` paths against that environment's base URL and validate absolute URLs against its allowlist (`ENV_FAILURE`, "url outside environment allowlist").
- `read_only: true` refuses any scenario whose mutation is not read-only, at every entry point: CLI exit 2, dashboard HTTP 400, job FAILED with the reason.
- Agent work (discover, verify, repair, reproduce) and all cadence, soak and impacted scheduling stay on the default environment. Runs record their environment (`runs.environment`, `result.json`, incident md/json) and the dashboard shows it as a badge.

## Consequences

- Checking a fix or a report on another deployment is one flag, and the loop's own schedule is untouched.
- The blast radius of production is bounded by data already in the scenario, not by an operator remembering which target is loaded.
- Multi-environment *scheduling* is deliberately out of scope: nothing runs a scenario on two environments automatically. Per-environment `asset_page` is accepted and validated, but the deployment gate still probes `deployment.readiness.asset_page` on the default environment only.
- A scenario that mutates data cannot be exercised on a read-only environment at all, not even by an operator who is sure. That is the point; use a non-read-only environment.
