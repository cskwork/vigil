# ADR-0004: Queue-originated scripts wait for human approval

## Status

Accepted

## Date

2026-09-10

## Context

vigil now ingests three kinds of signal. A `ship` feature (git commit, sdlc-kit work item, feature file) is a deployment: the expected behaviour is whatever the change intended, and a scenario written for it is proven by validation plus clean SOAK runs against a target that keeps moving. An `issue` (Jira) or `log` (Loki, exec) feature is not a deployment. It is a report: someone says the product misbehaves, or a log signature says it does. The agent's job there is to reproduce the symptom and encode the *expected* behaviour, which it can only infer from prose written by a human who is not in the room.

Two failure modes follow. A script that reproduces a real bug fails on purpose: promoting it automatically would open a regression incident every cadence tick for a known, already-tracked defect. A script that misreads the report passes on purpose and pins the wrong behaviour as an oracle. Neither is visible to the SOAK gate, which only measures whether a script is stable.

Reproduction results also need to reach the person who filed the issue. Our team reads Jira, not a QA dashboard.

## Decision

- Candidates whose source feature is not `ship` end their validation run in `PENDING_APPROVAL` (PASS or APP_FAILURE alike) with the verdict stored on the scenario as `reproduction` JSON (`reproduced`, `at_step`, `symptom`, `run_id`). `SCRIPT_DRIFT` and friends keep the existing bounded repair loop; exhausted repairs still land in `NEEDS_REVIEW`. Coverage-exploration and ship-feature candidates keep SOAK → automatic ACTIVE.
- `PENDING_APPROVAL` is never due. Manual runs from the CLI or the dashboard record the run and its metrics but change no state, open no incident and queue no repair, so a human can re-run a pending script as often as they like while judging it.
- `vigil approve <id>` (and the dashboard `✔ 승인` button) moves it to `ACTIVE` with `cadence: daily` and a next due at `schedule.daily_at`. Approval is what converts "this reproduces a bug" into "watch this every day until it stops reproducing"; a P1 cadence would spam a known defect, and a soak counter would measure nothing new. `approve --soak` still opens the old path for a script that a reviewer wants treated as ordinary coverage.
- When the source is a Jira issue, approval posts a short Korean receipt back to the issue with acli and then reads the comments back, because acli writes have been observed to report success without effect. `jira.dry_run` writes the same body to `evidence/jira/<KEY>-<ts>.md` instead. A failed comment never undoes the approval; it is reported in the receipt.

## Consequences

- No queue-originated script can gate anything without a named human action, and the audit trail of that action lives in the tracker the team already reads.
- Approvals are a real queue: unapproved scripts accumulate and are visible in `list --state PENDING_APPROVAL`, `status` and the dashboard filter `승인 대기`.
- `jira.comment_on_approve: false` (or a non-Jira source ref) keeps everything local; the approval itself is unchanged.
- A daily cadence is coarse. A script that needs the old per-class cadence must be approved with `--soak`, or its cadence changed later.
