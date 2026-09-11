# ADR-0006: Model fallback chain with cooldown

## Status

Accepted

## Date

2026-09-10

## Context

A single `agent.model` made every Browser Agent task depend on one provider's quota. In this checkout `zai/glm-5.3-flash` answers 429 on quick successive calls, so `budget.agent_tasks_per_hour` had to be lowered to 4 and `agent.backoff` raised to 60s: an entire hour of agent work could be lost to one provider's rate limiting while three other keys sat idle. Retrying the same model with linear backoff is the wrong medicine for a quota error, which does not clear in 45 seconds, and it is the only medicine for a transient network error, which does.

Rule 12 is not negotiable: the deterministic loop must keep working when every model is down.

## Decision

- `agent.models` is an ordered chain of `provider/model[:thinking]` entries; `agent.model` remains the single-entry legacy form. `agent.Chain` holds the parsed entries and an in-memory `cooldownUntil[provider/model]` map guarded by a mutex; `Next(exclude...)` returns the first entry that is neither cooling down nor excluded.
- A provider error (429, quota, auth, 5xx, "rate limit") switches models **only when the chain currently offers an alternative**. Then the failed entry is put on cooldown for `agent.model_cooldown` (default 10m), the next entry starts immediately with a fresh retry budget and no backoff, and the loop prints `agent: <model> unavailable (<reason>); switching to <next>`. Switching is free; waiting is not.
- With no alternative available the old behaviour is exactly preserved: a transient error retries the same entry `agent.retries` times with `agent.backoff`, an auth/quota error does not retry, and when the retries are gone the entry is cooled down and the result is `ModelUnavailable` — rule 12, no exception.
- Cooldown state is per process and in memory: it is a rate-limit heuristic, not durable state, and a restart is allowed to try everything again.
- `agent-result.json` records `model` (the entry that produced the result) and `model_attempts` (`{model, outcome}` per spawn, outcome one of `ok`, `unavailable`, `error`, `timeout`, `budget`, `no_result`). `vigil doctor` prints one line per chain entry from `pi auth check --provider <p> --json --no-refresh` and warns — never fails — for entries that are not ready, because the chain simply skips them.

## Consequences

- One provider's quota no longer stops agent work, and `budget.agent_tasks_per_hour` can be raised on a chain of several providers.
- Results become model-dependent: `model_attempts` in the evidence is what explains why two runs of the same task differ.
- A misconfigured entry fails at startup (`vigil validate` / chain construction), not on the first agent task.
- Nothing changes for a single-entry configuration, so the upgrade is opt-in.
