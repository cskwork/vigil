<div align="center">

# vigil

**Continuous browser QA that grows its own coverage.**

Point it at a deployed web app. It watches your ships, proves them in a real browser,
files evidence for every run — and a supervised AI loop keeps writing new test
scenarios until every reachable flow has one.

[![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?logo=go&logoColor=white)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)
[![Runs on](https://img.shields.io/badge/browsers-Lightpanda%20·%20Chromium-orange)](#architecture)

</div>

---

## Why vigil

Most E2E suites test what somebody remembered to write, break when the page
changes, and say "assertion failed" without evidence. vigil inverts each of
those:

- **Coverage grows itself.** A supervisor loop reads the QA state on a slow
  tick, asks a model where scenarios are missing, and turns the answer into
  browser explorations. Give it one URL; the route map fills itself in from
  wherever browsers actually go.
- **The model proposes, code decides.** Every action the model suggests is
  re-validated by deterministic Go before it becomes a job. There is no verb
  for deleting coverage — asking for one is refused and logged. A model outage
  never stops deterministic QA.
- **Broken scripts repair themselves.** A failed validation gets bounded agent
  rewrites (`policy.agent_fix_attempts`) before a human ever sees it. Brittle
  locator? The agent reads the failure and rewrites the step.
- **Every run leaves evidence.** Console, network, DOM and screenshots per run;
  failures become reproducible incident reports, not a red X.
- **Deploy-aware.** Scenarios re-run the moment a shipped commit is actually
  served (asset-version gate), so "did the deploy break it?" has an answer
  minutes after the deploy.

## Give it one URL

```yaml
target:
  base_url: https://your-app.example.com
supervisor:
  enabled: true
```

Then watch it work:

```text
loop: supervisor every 15m0s (model=zai/glm-5.3-flash dry_run=false max_actions=5)
supervisor: discover_route "/app" applied (7 of 7 scenarios live on the entry
            route; the interior is where the next coverage gap is)
supervisor: 2 action(s) proposed, 2 applied, 0 refused, 2 gap(s) named
supervisor: 6 new route(s) discovered (23 known)
scenario home-deep-link-requires-auth: validation failed; agent fix attempt 1/3
agent job 12: candidate teacher-home-dashboard → SOAK (validated by runner)
```

Within an hour of first launch against a real application, vigil discovered 23
routes and authored 16 scenario candidates from a single seeded path — pinning
one test identity so every scenario reproduces, filling forms where the flow
allows it, and staying inside the host allowlist.

## Safety rails

The supervisor is deliberately the *weakest* actor in the system:

| Rail | Effect |
|---|---|
| Verb allowlist | 6 verbs only; `delete`/`retire`/`purge` refused **by name** and logged |
| Host allowlist | An off-target URL is rejected outright, at harvest and at validation |
| Deny path patterns | `logout`, `delete`, `purge`… are never explored — a wandering browser must not end its session or press something irreversible |
| Mutation policy | `read-only` → `reversible` → `destructive` (off unless `policy.allow_destructive`) |
| Budgets | Hourly task budgets per actor; the planner cannot starve discover/repair |
| Active hours | A weekly window (e.g. weekdays 08:00–19:00) gates all cadence work, editable live from the dashboard |
| Quarantine guard | The one coverage-reducing verb is capped per plan and refused if it would blind an area completely |
| Oracle policy | Observed behaviour either waits for a human (`needs_review`) or must survive validation + clean SOAK runs (`soak`) before it gates anything |

## Quickstart

```bash
git clone https://github.com/cskwork/vigil
cd vigil && go build ./cmd/vigil
./vigil doctor --install                 # fetches the Lightpanda browser
cp vigil.example.yaml vigil.yaml         # point target.base_url at your app
./vigil loop --ui 127.0.0.1:8787         # scheduler + live dashboard
```

The dashboard at `:8787` shows verification results, running jobs, the
coverage-growth panel and the schedule window. Its main page also has one text
box for an operator to describe a QA situation. Vigil forces these submissions
to `read-only` and gives them priority 110, above repair and recent-failure work
at priority 100.

If the same `loop --ui` process is running another Browser Agent job, it cancels
that job cooperatively, returns it to `READY` without charging an attempt or
Agent budget, and runs the operator request next. Deterministic scenario workers
continue uninterrupted. The operator request may bypass the hourly Agent budget
once. Any scenario it proposes still passes the existing candidate validation,
deduplication, oracle, and SOAK gates.

Dashboard controls are unauthenticated. Bind `loop --ui` and `serve` only to a
trusted address such as `127.0.0.1`. Standalone `serve` shows the QA box but
keeps submission disabled; use `loop --ui` with an available Browser Agent to
submit a request.

## Status

The full local product is implemented and exercised end to end against a real
deployment: git feature ingest, deployment gate, deterministic
Lightpanda/Chromium runner, failure classification, candidate lifecycle,
Browser Agent (pi + nono sandbox), the autonomous coverage supervisor,
scheduler with budgets/locks/active-hours, evidence/incidents, CLI and the live
web board. Accepted by the config but ignored at runtime: `workers.chromium`,
`budget.chromium_minutes_per_hour` enforcement (minutes are recorded),
`runtime.mode: distributed`, `state.type: postgres`, `evidence.type: s3`.

## Architecture

```text
Git ship/change
      │
      ▼
 deployment-ready?
      │
      ▼
┌─────────────────────────┐
│      Orchestrator       │
│                         │
│ existing coverage?      │
│ script confidence?      │
│ changed/impacted area?  │
│ recent failure?         │
└──────┬───────────┬──────┘
       │           │
  uncertain      proven
       │           │
       ▼           ▼
 Browser Agent   QA Script
   subagent       Runner
       │           │
       └─────┬─────┘
             ▼
        Lightpanda
             │
             ▼
      deployed website
             │
       PASS / classified FAIL
             │
       browser ambiguity
             ▼
          Chromium
```

| Package | Responsibility |
|---|---|
| `cmd/vigil` | CLI: global flags, command dispatch, `doctor` checks |
| `internal/ingest` | Turns `generic-git`, `file` and `sdlc-kit` signals into normalized `FeatureEvent`s |
| `internal/gate` | Deployment readiness gate (`delay`, `asset_version`, `version_endpoint`) |
| `internal/orchestrator` | Decision table, impact selection, post-run lifecycle (soak/promote, quarantine, repair, Chromium confirm, incidents), agent job handling, coverage supervisor (plan → validate → apply, stale-review reconcile) |
| `internal/scheduler` | Continuous loop: scan/gate/orchestrate, due selection, budgets, locks, workers, cheap retry, PASS marker |
| `internal/runner` | Executes DSL steps over CDP; captures console, network, DOM, screenshot |
| `internal/browser` | Lightpanda (`serve` attach-or-launch) and Chromium providers |
| `internal/classify` | Maps runner results to PRD §13 outcomes |
| `internal/dsl` | Scenario/flow schema, validator, logical fingerprint |
| `internal/evidence` | Run directories, PASS marker, portable incidents, retention prune |
| `internal/store` | SQLite (WAL) state: features, scenarios, versions, jobs, runs, incidents, locks, budget |
| `internal/agent` | pi Browser Agent adapter, task contract, secret redaction, nono sandbox |
| `internal/config` | `vigil.yaml` + `url.md` loading, defaults, `${ENV}` substitution |
| `internal/model` | States, outcomes, actions, job kinds, priorities, entities |

## Quickstart on this repo

Assumes `bin/lightpanda` is present (it is gitignored). All commands run from the repo root.

```bash
cd <your-home>
go build ./cmd/vigil
./vigil doctor                      # sqlite, browsers, pi, keys, sandbox, target, evidence dir
./vigil validate                    # every scenario/flow file on disk
./vigil add                         # register project, import scenarios/ + flows/ into the store
./vigil scan                        # ingest features once, gate, orchestrate
./vigil run home-landing  # one scenario now
./vigil loop                        # continuous scheduler + workers until SIGINT
```

Global flags may appear anywhere: `-c <config>` / `--config=<path>` (default `vigil.yaml`) and `--json` (honoured by `status`, `coverage`, `incidents`, `list`, `show`, `doctor`).

## Portable setup on a new machine

```bash
git clone <this repo> vigil && cd vigil
go build ./cmd/vigil
./vigil doctor --install            # downloads the Lightpanda nightly for this OS/arch into bin/lightpanda when missing
                                      # (this build prints the release URL; download manually and chmod +x)
```

Then:

1. Edit `url.md`. The first URL is the primary target: its origin becomes `target.base_url`, its host is allowlisted, and its path becomes the default entry route.
2. Edit `vigil.yaml`: `project.repo` (git checkout of the deployed app) and the `discovery` section (branch, `path_prefixes`, `route_map`).
3. Export the API key named on the right-hand side of `agent.env_map` (here `Z_AI_API_KEY`); pi receives it under the left-hand name (`ZAI_API_KEY`).
4. Chromium: leave `browser.chromium.binary` empty when you do not need it, or point it at a Playwright-cache or system Chrome binary.
5. pi extension: leave `agent.extension` empty; vigil loads its own dependency-free `piext/vigil-browser.js` (it exposes `agent_browser` by spawning the `agent-browser` CLI, and it is what runs inside the nono sandbox). Only if that file is missing does it fall back to `pi-agent-browser-native` under `$HOME/.pi/agent/npm/node_modules` / `npm root -g`.
6. `./vigil add`, then `./vigil loop`.

`url.md`, `vigil.yaml`, `scenarios/`, `flows/` and `features/requests/` are domain-specific and git-ignored: copy `url.example.md` to `url.md`, `vigil.example.yaml` to `vigil.yaml`, and start from `examples/`. The binary and the DSL are generic. `vigil.yaml` must not contain user-absolute paths: use `${HOME}` or leave `browser.chromium.binary` / `agent.extension` empty. `.vigil/`, `evidence/`, `bin/` and the built binary are gitignored.

## CLI reference

| Command | Semantics |
|---|---|
| `init` | Write a starter `vigil.yaml` and `url.md` next to it (only if missing); create `scenarios/`, `flows/`, `features/` |
| `add [project]` | Upsert the project (must equal `project.id`) and import `scenarios/` + `flows/` from disk (seeds enter SOAK) |
| `import --history [--limit N]` | Ingest the last `discovery.history_limit` (or N) historical features; no gate, no plan |
| `scan` | Import files, poll the discovery adapter once, gate unhandled features, orchestrate READY ones, print feature table |
| `discover <feature>` | Enqueue an `AGENT_DISCOVER` job for a known feature and run it inline (requires the Browser Agent) |
| `run <scenario\|feature> [--browser lightpanda\|chromium]` | Run one scenario inline; a feature id runs every ACTIVE/SOAK scenario linked to it |
| `run --feature <f>` | Run every ACTIVE/SOAK scenario with a `feature` coverage link to `f` |
| `run --impacted <f>` | Run scenarios impacted by `f` (feature, then capability via `route_map`, route, path links) |
| `run --all` | Run every ACTIVE/SOAK scenario |
| `loop` | Continuous scheduler + `workers.functional` run workers + one agent worker; logs to stdout and `<state dir>/vigil.log` |
| `status` | Scenario/job counts, last-hour budget, features, queue, next due, recent runs, open incidents |
| `coverage` | Per-scenario coverage links, metrics and corpus health (duplicate fingerprints, quarantined, never run, needs review, flakiest) |
| `incidents [--all] [--limit N]` | Open (or all) incidents with Markdown paths |
| `approve <scenario>` | NEEDS_REVIEW / CANDIDATE / QUARANTINED / REJECTED to SOAK; soak counter and failures reset; due now |
| `reject <scenario>` | Any state to REJECTED; resolves its open APP_REGRESSION incidents |
| `list [--state A,B]` | List scenarios, optionally filtered by comma-separated states |
| `show <scenario>` | Current script YAML, coverage links, metrics, last 10 runs |
| `validate` | Parse and validate every scenario/flow file; flags duplicate ids and duplicate fingerprints; exit 1 on problems |
| `serve [--addr 127.0.0.1:8787]` | Standalone live web view for non-developers. QA submission stays disabled because no in-process scheduler owns the request; existing schedule controls remain available |
| `loop --ui <addr>` | `loop` plus the live view and one-box, read-only QA submission. An available Browser Agent is required |
| `request <file.yaml> [--queue] [--dry-run]` | Hand a manual QA request to the orchestrator: `feature_id`, `summary`, `entry_url`, `accounts` (named test accounts, no secrets), `instructions` (the flow in plain language), `mutation` (read-only / reversible / destructive), `locks`, `max_tool_calls`, `timeout_minutes`. The Browser Agent performs the flow with one isolated browser session per role (`"session":"teacher"`) and proposes scripts; `--queue` leaves it for a running loop |
| `doctor [--install]` | Check config/targets, sqlite, evidence dir, repo/branch, Lightpanda binary + serve, Chromium, pi, API keys, extension, nono, agent, target HTTP, asset marker |

`run` exits 1 when any scenario did not PASS. `doctor` exits 1 only on hard failures (sqlite, evidence dir, repo, target).

## Config reference

`vigil.yaml` is read with `${VAR}` substitution (unset variables become empty). Relative paths resolve against the config file directory. Defaults come from `applyDefaults`.

### runtime / project

| Key | Default | Meaning |
|---|---|---|
| `runtime.mode` | `local` | `local` or `distributed`; this build opens SQLite and local evidence regardless |
| `project.id` | required | Project key for all state |
| `project.repo` | | Git checkout of the deployed app; required by `generic-git` and `sdlc-kit` |

### target (+ url_file)

| Key | Default | Meaning |
|---|---|---|
| `target.base_url` | derived | Origin of the primary URL when empty; required otherwise |
| `target.allowed_hosts` | derived | Primary host is appended when not already allowed; match is exact or subdomain suffix |
| `target.url_file` | `url.md` when present | One URL per line, `<label> \| <url>` or bare `<url>`, `#` comments |

The first URL is the primary target: origin becomes `base_url`, host is allowlisted, and its path (`EntryPath`) is the default entry route for the asset probe and agent discovery. Extra lines are additional routes the agent may explore.

### discovery

| Key | Default | Meaning |
|---|---|---|
| `adapter` | `file` | `generic-git`, `sdlc-kit` or `file` |
| `branch` | `HEAD` | `generic-git`: ref to watch, e.g. `origin/staging` |
| `issue_key_pattern` | `\b[A-Z][A-Z0-9]+-\d+\b` | regexp that finds the tracker key in a commit subject. Case-sensitive by default so `utf-8` is not mistaken for a key; prefix with `(?i)` to accept lower case |
| `fetch` | `false` | `git fetch origin <branch>` before each poll (only when branch starts with `origin/`) |
| `path_prefixes` | all | Only first-parent commits touching these prefixes become features |
| `route_map` | | `[{path_prefix, route, capability}]`; maps changed paths to routes and capabilities |
| `features_dir` | `features` | `file` adapter: directory of `FeatureEvent` YAML files |
| `poll_interval` | `60s` | Loop poll cadence |
| `history_limit` | `5` | `import --history` and first-poll window |

`generic-git` groups commits by `issue_key_pattern` in the subject (`PROJ-123`, upper-cased) or `commit-<7 sha>`; the newest commit supplies sha, time and summary, paths/routes are the union. Each feature is delivered once per shipped sha.

`file` adapter event (one file per feature under `features_dir`, ingested once per `shipped_sha`):

```yaml
feature_id: remedy-page-order
status: shipped
shipped_sha: abc123
shipped_at: 2026-09-01T03:20:00Z   # RFC3339; omitted = file mtime
changed_paths:
  - src/pages/remedy/OctoPlayer.vue
routes:
  - /remedy
summary: Remedy display ordering changed.
```

`sdlc-kit` adapter convention: every directory under `<project.repo>/.sdlc/work/` is one work item whose `STATE.md` (or `state.md`) carries `key: value` lines. Keys are case-insensitive; leading markdown bullets/headings and emphasis or backticks around key and value are ignored; prose lines (key with spaces) are skipped; the first occurrence of a key wins. An item is shipped when it has `status: shipped` or `phase: SHIP`.

```text
feature_id | feature | id   feature id (default: the directory name)
sha | shipped_sha           shipped commit (required; shipped items without it are skipped)
shipped_at                  RFC3339 (default: STATE.md modification time)
summary | title             (default: the feature id)
changed_paths, routes       comma-separated lists (optional)
```

### deployment.readiness

| Key | Default | Meaning |
|---|---|---|
| `strategy` | `delay` | `delay`, `asset_version` or `version_endpoint` |
| `delay_after_ship` | `2m` | `delay`: READY once `now >= shipped_at + delay_after_ship` |
| `asset_page` | `base_url` + first route | `asset_version`: page whose `assets/index.js?v=<n>` marker changes on deploy |
| `endpoint` | required for `version_endpoint` | URL whose body must contain the first 7 chars of the shipped SHA |
| `max_wait` | `30m` | After this, `asset_version` / `version_endpoint` answer `DEPLOYMENT_UNKNOWN` |

| Strategy | READY | WAITING_FOR_DEPLOYMENT | DEPLOYMENT_UNKNOWN |
|---|---|---|---|
| `delay` | delay elapsed | before that | never |
| `asset_version` | marker differs from the one seen when the SHA was first gated | first sight, unchanged marker, or probe error | unchanged after `max_wait` |
| `version_endpoint` | body contains SHA prefix | no match, or probe error | no match after `max_wait` |

Network failures never error: the gate answers WAITING and logs. The scheduler treats `DEPLOYMENT_UNKNOWN` like READY (delay policy expired, proceed). A deployment delay is never `APP_FAILURE`.

### browser / workers

| Key | Default | Meaning |
|---|---|---|
| `browser.primary` | `lightpanda` | Default functional browser |
| `browser.fallback` | `chromium` | Not read. The confirmation browser is always Chromium (`scheduler.pickBrowser`); the key is accepted for compatibility and ignored |
| `browser.lightpanda.binary` | `bin/lightpanda` | Launched as `serve --host H --port P`; an already healthy server on H:P is attached instead |
| `browser.lightpanda.host` / `.port` | `127.0.0.1` / `9333` | Never `9222` |
| `browser.chromium.binary` | `""` | Explicit Chromium / headless-shell path |
| `browser.chromium.headless` | `false` | |
| `browser.step_timeout` | `15s` | Per step |
| `browser.run_timeout` | `3m` | Per run; lock TTL is twice this |
| `workers.functional` | `1` | Run workers in `loop` |
| `workers.chromium` | `0` | Accepted; not used by the local scheduler |

### state / evidence

| Key | Default | Meaning |
|---|---|---|
| `state.type` / `.path` / `.dsn_env` | `sqlite` / `.vigil/state.db` / | SQLite WAL, single writer |
| `evidence.type` / `.dir` / `.bucket` | `local` / `evidence` / | Evidence root |
| `evidence.retain_pass_days` | `3` | Prune PASS run dirs older than this |
| `evidence.retain_fail_days` | `30` | Prune failed run dirs and agent dirs older than this |

### budget / policy / schedule

| Key | Default | Meaning |
|---|---|---|
| `budget.browser_minutes_per_hour` | `60` | At 100% all due work is deferred; at 80% clean P2 work is deferred |
| `budget.chromium_minutes_per_hour` | `5` | Recorded and shown in `status` |
| `budget.agent_tasks_per_hour` | `10` | Agent worker idles and new agent decisions become `NO_ACTION` once spent |
| `policy.allow_destructive` | `false` | Destructive scenarios go to NEEDS_REVIEW without running |
| `policy.soak_passes` | `3` | Clean SOAK runs before ACTIVE |
| `policy.quarantine_after` | `3` | Consecutive flakes before QUARANTINED |
| `policy.retry_on_fail` | `1` | One cheap retry for read-only scenarios |
| `policy.observation_oracle` | `needs_review` | `needs_review` or `soak` for observation-only candidates |
| `policy.structural_dup_threshold` | `0.8` | Jaccard similarity of major actions above which a candidate is DUPLICATE |
| `schedule.soak` / `p0` / `p1` / `p2` | `10m` / `15m` / `60m` / `6h` | Cadence by state/class |
| `schedule.failure_backoff` | `5m` | Next due after any non-pass |
| `schedule.tick` | `10s` | Due selection and lease reaping |

### agent

| Key | Default | Meaning |
|---|---|---|
| `provider` | `pi` | |
| `model` | `zai/glm-5.3-flash` | `<provider>/<model>` |
| `thinking` | `high` | |
| `timeout` | `20m` | Per agent task |
| `extension` | `piext/vigil-browser.js` | pi extension exposing `agent_browser`; bundled file by default, pi-agent-browser-native as fallback |
| `env_map` | | `{PI_VAR: SHELL_VAR}`; `doctor` warns when SHELL_VAR is empty |
| `retries` / `backoff` | `3` / `45s` | Transient errors (rate limit, 429, 5xx, timeouts) retry with linear backoff |
| `max_turns` | `40` | Tool-call budget; exceeding it yields NEEDS_REVIEW |
| `workdir` | | Not read. The agent runs in the evidence directory for the task; the key is accepted and ignored |
| `sandbox` | `auto` | `auto` (nono when on PATH), `nono`, `none` |
| `sandbox_network_filter` | `false` | Use `nono run` with a proxy allowlist of `target.allowed_hosts` |
| `max_scenarios_per_task` | `3` | Candidate scenarios per agent task |

### evidence retention

Evidence never grows unbounded. Hourly (and at loop start) vigil applies: pass runs older than `retain_pass_days` (3) and failures/agent runs older than `retain_fail_days` (30) are deleted; then `max_total_mb` (1024) is enforced by deleting the oldest pass runs, then the oldest agent runs, then the oldest failures until under the cap, always keeping the newest entry per scenario and per feature. Run rows older than `retain_runs_days` (90) leave the database unless an incident references them. Agent transcripts skip streaming deltas and the pi session copy is removed after the task. `vigil prune [--dry-run]` applies the policy now; `vigil status` shows the footprint.

### evidence.chromium_capture

`per-deploy` (default): after a scenario passes on Lightpanda, vigil runs it once more on Chromium for each new deployment marker so the verification report carries a real rendered screenshot (Lightpanda does not paint; its captures are text renderings). Background priority, one Chromium run per scenario per build. `off` disables it.

This needs a deployment marker, and only `deployment.readiness.strategy: asset_version` produces one. Under the default `delay` strategy the marker is always empty, so the Chromium capture never runs and every screenshot stays a Lightpanda text rendering — `steps.json` records `capabilities.screenshot_painted: false` when that is the case.

### personas / paths

```yaml
personas:
  qa_student:
    username: ${QA_STUDENT_USER}
    password: ${QA_STUDENT_PASSWORD}
    extra: { school: "..." }
paths:
  scenarios: scenarios
  flows: flows
```

Persona values are passed to the runner as a credential map and redacted from agent evidence.

## Scenario DSL reference

```yaml
scenario:
  id: visible-remedy-order          # ^[a-z0-9][a-z0-9._-]{1,79}$
  version: 3                        # >= 1
  title: Remedy pages are ordered
  class: P1                         # P0 | P1 | P2 (default P1)
  mutation: read-only               # read-only | reversible | destructive (default read-only)

covers:
  feature: remedy-page-order
  capability: remedy.navigation
  routes: [/remedy]
  apis: [/api/remedy]
  paths: [src/pages/remedy/OctoPlayer.vue]

uses: [login-as-student]            # flows expanded before steps

preconditions:
  persona: qa_student

resources:
  locks: [qa_student_01]            # required when mutation != read-only

browser:
  primary: lightpanda               # lightpanda | chromium
  requires_chromium: false          # rendering/layout is part of the oracle
  popup: true                       # scenario opens new pages

steps:
  - goto: /remedy/current
  - name: next page
    click: { by: role, role: button, name: Next }
  - assert_text: { value: "2" }

assert:
  no_uncaught_console_error: true
  no_http_5xx: true
  no_http_4xx_on: [/api/remedy]     # URL substrings

oracle:
  source: spec                      # spec | approved_qa | contract | observation
  source_feature: remedy-page-order
  source_sha: abc123
  note: acceptance criteria AC-3
```

### Steps

Exactly one action per step; `name` is an optional label.

| Action | Argument shape |
|---|---|
| `goto` | `/path` or absolute `http...` URL |
| `click`, `hover`, `wait_for`, `assert_visible`, `assert_not_visible` | Locator |
| `fill`, `type` | Locator fields + `input` |
| `press` | key string |
| `select` | Locator fields + `option` |
| `wait_ms` | integer milliseconds |
| `wait_url`, `assert_url` | `{ contains }` or `{ matches: <regexp> }`, optional `timeout` |
| `assert_text`, `assert_no_text` | `{ value, in?: Locator, exact? }` (default scope: document body) |
| `assert_count` | Locator fields + `equals` / `min` / `max` |
| `assert_request` | `{ url_contains, method?, status?, status_min?, status_max?, body_contains?, timeout? }` |
| `assert_attr` | Locator fields + `attr` + `equals` / `contains` |
| `expect_popup` | `{ url_contains?, timeout? }`; switches the run to the page opened by the previous action |
| `eval` | `{ script, expect? }`; `expect` is a JSON-encoded value, empty = just run |
| `use_flow` | flow id (not allowed inside flows) |
| `screenshot` | file name |

### Locators

| `by` | Needs | Matches |
|---|---|---|
| `test_id` | `value` | `[data-testid=value]` or `[data-test-id=value]` |
| `role` | `role` (+ optional `name`) | ARIA role with accessible name |
| `label` | `name` (or `text`/`value`) | `<label>`, `aria-label`, `aria-labelledby`, `placeholder` |
| `id` | `value` | element id |
| `text` | `text` (or `value`) | visible text, substring unless `exact` |
| `href` | `value` | `a[href]`/`area[href]` containing value (equal when `exact`) |
| `css` | `value` | CSS selector |
| empty | any of `value`, `text`, `name`, `role` | strategy inferred from the field set |

Common fields: `exact` (bool), `nth` (0-based), `timeout` (e.g. `10s`).

```yaml
click: { by: text, text: 중학, exact: true }
click: { by: css, value: ".x", nth: 0 }
click: { by: role, role: button, name: Next }
```

### Flows

```yaml
flow:
  id: open-training-entry
  version: 1
  title: Open the training entry page and wait for the school tabs
steps:
  - goto: /app/entry
  - wait_for: { by: css, value: ".school-btn-wrap button", timeout: 15s }
```

Use a flow either with `uses: [id]` (runs before `steps`) or inline with `- use_flow: id`. Flows cannot nest `use_flow`.

### Validation rules

`validate` (and every import) rejects a scenario when: the id or version is invalid; `class`, `mutation`, `browser.primary` or `oracle.source` has an unknown value; `oracle.source` is missing; `steps` is empty; a step has zero or more than one action; a locator lacks its required field; `select` lacks `option`, `assert_count` lacks `equals`/`min`/`max`, `assert_request` lacks `url_contains`, `assert_attr` lacks `attr`, `eval` lacks `script`, `assert_url`/`wait_url` lack `contains`/`matches` or `matches` is not a valid regexp, `goto` is neither a path nor a URL; a `reversible`/`destructive` scenario has no `resources.locks`; there is no assertion at all (a step `assert_*` or a scenario-level `assert` flag); or `uses`/`use_flow` references an unknown flow. Unknown YAML keys are errors. Duplicate ids fail; duplicate fingerprints warn.

### Fingerprint

The logical-scenario fingerprint hashes: `covers.capability`, `preconditions.persona`, sorted `covers.routes`, sorted `covers.apis`, the sequence of major actions, and the `no_uncaught_console_error` / `no_http_5xx` flags. Flows are expanded, so a scenario using a flow equals its inlined form. Waits, hover and screenshots are ignored. Locator mechanics are ignored: a locator reduces to its `name` or `text`, else its `value` with `-`, `_`, `#`, `.`, brackets, quotes and `data-testid=` stripped, else its `role`. `click text="Submit"`, `click test-id="submit"` and `click role=button name="Submit"` all normalize to `click:submit`. `goto` drops the query string.

## Lifecycle and outcomes

### Scenario states

| State | Meaning |
|---|---|
| `CANDIDATE` | Agent-proposed, not yet validated |
| `SOAK` | Validated (or seeded/approved), accumulating `policy.soak_passes` clean runs; scheduled |
| `ACTIVE` | Promoted regression coverage; scheduled by class cadence |
| `DUPLICATE` | Same fingerprint or structurally similar to an existing scenario |
| `EPHEMERAL` | Exploratory only; never persisted as regression |
| `NEEDS_REVIEW` | Human decision required (oracle unknown, observation-only, destructive without policy, inconclusive Chromium confirm) |
| `REJECTED` | Rejected by `reject` |
| `QUARANTINED` | `policy.quarantine_after` consecutive flakes |
| `MERGED`, `SUPERSEDED`, `RETIRED` | Corpus governance states |

Only `ACTIVE` and `SOAK` scenarios are selected as due. Files imported from `scenarios/` enter SOAK. `vigil approve <id>` moves NEEDS_REVIEW / CANDIDATE / QUARANTINED / REJECTED to SOAK with the soak counter and consecutive failures reset and the scenario due now; after `policy.soak_passes` clean runs the next PASS promotes it to ACTIVE. `vigil reject <id>` sets REJECTED and resolves its open regression incidents.

### Outcomes and what happens next

| Outcome | Meaning | Next |
|---|---|---|
| `PASS` | All steps and scenario asserts held | PASS marker; SOAK counter +1 and SOAK to ACTIVE at target; next due by cadence; open APP_REGRESSION incidents resolved |
| `QA_FLAKE` | Failed once, passed on the cheap retry | Flake counter +1; QUARANTINED at `quarantine_after`; cadence |
| `SCRIPT_DRIFT` | Locator/navigation no longer matches | `AGENT_REPAIR` job (if agent available); `failure_backoff` |
| `LIGHTPANDA_INCOMPATIBLE`, `BROWSER_AMBIGUOUS` | Lightpanda cannot be trusted for this script | `CHROMIUM_CONFIRM` job; `failure_backoff` |
| `APP_FAILURE` | Business assertion failed | One OPEN APP_REGRESSION incident (md + json); regressions counter; re-run at `failure_backoff` with top priority |
| `AUTH_FAILURE`, `DATA_FAILURE`, `ENV_FAILURE`, `DEPLOYMENT_NOT_READY` | Infrastructure, not product | One OPEN ENVIRONMENT incident (shared); `failure_backoff`; no consecutive-failure bump |
| `ORACLE_UNKNOWN`, `NEEDS_REVIEW` | Correctness cannot be established | State NEEDS_REVIEW |

Retry: only read-only scenarios get one cheap retry (`policy.retry_on_fail`) before classification; mutating scenarios never retry. Chromium confirm: PASS on Chromium adds a new script version with `browser.primary: chromium` (mechanics only, `created_by: system`) and counts as PASS; a failed assertion on Chromium becomes APP_FAILURE "confirmed on chromium"; infrastructure outcomes open the environment incident; anything else goes to NEEDS_REVIEW. Budget: when browser minutes reach 100% every due scenario is deferred, at 80% clean P2 scenarios are deferred; the agent worker idles while `agent_tasks_per_hour` is spent.

### Feature decisions

For a READY (or DEPLOYMENT_UNKNOWN) feature the orchestrator picks: `RUN_IMPACTED_SCRIPTS_FIRST` when ACTIVE/SOAK scenarios are linked by feature, capability (via `route_map`), route or changed path; otherwise `BROWSER_AGENT_DISCOVER` when the agent is available and within budget; otherwise `NO_ACTION`. When all impacted runs finish, clean results keep coverage; any APP_FAILURE, SCRIPT_DRIFT, BROWSER_AMBIGUOUS, LIGHTPANDA_INCOMPATIBLE, NEEDS_REVIEW or ORACLE_UNKNOWN enqueues `AGENT_VERIFY` (agent and budget permitting). The feature is marked handled for that SHA.

### Priorities and job kinds

| Priority | Value | When |
|---|---|---|
| Operator QA request | 110 | Main-page read-only request; runs before repair work |
| Recent failure | 100 | `consecutive_failures > 0`, inline `run`, repair, Chromium confirm, post-failure re-run |
| New direct coverage | 90 | `AGENT_DISCOVER` / `AGENT_VERIFY` jobs |
| Soak | 80 | SOAK scenarios |
| Impacted | 70 | Impacted runs after a feature ships |
| P0 / P1 / P2 | 60 / 50 / 40 | Cadence runs by class |
| Background | 10 | Reserved |

Job kinds: `RUN_SCENARIO`, `AGENT_DISCOVER`, `AGENT_VERIFY`, `AGENT_REPAIR`, `CHROMIUM_CONFIRM`, `VALIDATE_CANDIDATE`. Job states: `READY`, `LEASED`, `DONE`, `FAILED`, `CANCELLED`. Leases last 5 minutes with a 30-second heartbeat; expired leases return to the queue every tick. Jobs are deduplicated by key (`run:<scenario>`, `impacted:...`, `agent:<feature>:<sha>`, `repair:...`, `chromium-confirm:...`), so a queued job is reused rather than duplicated.

## Browser Agent and the nono sandbox

- Rule 12: the loop never needs an LLM. When `pi` is missing or the model fails auth/quota, `loop` logs `Browser Agent unavailable; AGENT_* jobs stay READY, deterministic QA continues` and only agent jobs wait.
- `budget.agent_tasks_per_hour` bounds agent tasks; this checkout uses 4 because `zai/glm-5.3-flash` returns 429 on quick successive calls. Transient provider errors retry `agent.retries` times with `agent.backoff` linear backoff, then the task ends as model-unavailable without a verdict.
- On timeout / tool budget the persisted pi session is resumed once with a `WRAP UP` message (no tools) so an expensive exploration still yields a result block.
- `agent.sandbox: auto` wraps pi with `nono wrap` when nono is on PATH (network allowed, nono default). With `sandbox_network_filter: true` it uses supervised `nono run` and a proxy allowlist equal to `target.allowed_hosts` (more RAM). The application repo is never granted to the sandbox.
- Secrets (persona passwords, persona extras, secret-looking environment values) are redacted from the transcript, request, result and stderr before they are written as evidence.
- The task prompt forbids leaving `allowed_hosts`; URLs outside the allowlist seen in tool calls or visited pages are recorded as host violations and force the result to NEEDS_REVIEW.
- Agent results are proposals only. Persistent state changes happen after orchestrator validation (oracle provenance, dedup, independent script validation). Observation-only oracles follow `policy.observation_oracle`.

## Evidence layout

Under `evidence.dir`:

```text
runs/<scenario>/<UTC ts>-a<attempt>/   one attempt
    steps.json console.json network.json   always
    dom.html screenshot.png                on failure (when the browser can produce them)
    result.json                            compact run summary written by the scheduler
    PASS                                   marker written after a PASS classification
agent/<feature>/<UTC ts>/               one Browser Agent task
    agent-request.yaml agent-task-prompt.md agent-system-prompt.md agent-argv.txt
    agent-transcript.jsonl agent-result.yaml agent-result.json agent-stderr.log nono.log
incidents/<UTC ts>-<scenario>.md|json   portable incident for a coding agent
```

An incident carries: kind, project, outcome, reason, feature and shipped SHA, feature summary and changed paths, scenario id/version/title, browser, attempt, timing, failed step/action, expected vs actual, error text, console errors, failed or 5xx requests, artifact paths, Chromium confirmation excerpt, and the full scenario YAML for reproduction with `vigil run <scenario>`.

Retention: `Prune` runs at loop start and hourly. PASS run dirs older than `retain_pass_days` and failed run dirs older than `retain_fail_days` are deleted; agent dirs follow the failure retention; incidents are never pruned.

Logs: `loop` writes to stdout and `<state dir>/vigil.log` (state dir is the directory of `state.path`, here `.vigil/`). Browser process logs live in `<state dir>/browser-logs/`.

## Troubleshooting

| Symptom | Cause and fix |
|---|---|
| Lightpanda serve probe fails or attaches to the wrong browser | Port 9222 is Chrome's default remote-debugging port; never use it. vigil uses 9333. Check `lsof -i :9333`; a healthy server on 9333 is attached, otherwise `bin/lightpanda serve` is launched |
| Agent tasks end with `model unavailable after retries` / 429 | `zai/glm-5.3-flash` rate-limits quick successive calls. Keep `budget.agent_tasks_per_hour` at 4 and raise `agent.backoff` (this checkout: 60s) |
| Run classified `LIGHTPANDA_INCOMPATIBLE` or `BROWSER_AMBIGUOUS` | A `CHROMIUM_CONFIRM` job runs the same script on Chromium; a Chromium PASS adds a version with `browser.primary: chromium`. For rendering oracles set `browser.requires_chromium: true` in the scenario, or `browser.primary: chromium` in `vigil.yaml` to switch the default |
| `locks [...] held elsewhere; requeued=true ... (AC-16)` | Two mutating scenarios share a lock key. The job is requeued 30 seconds later (up to 5 attempts, then FAILED and the scenario backs off) |
| Feature shows `DEPLOYMENT_UNKNOWN` | `max_wait` elapsed without the asset marker or version endpoint changing. The gate proceeds as if READY; verify the deployment manually if results look stale |
| `warn: Browser Agent unavailable, continuing with deterministic QA only` | `pi` is not on PATH (`npm i -g @earendil-works/pi-coding-agent`), the extension is missing (`npm i -g pi-agent-browser-native`), or the API key env is empty. Run `vigil doctor` |
| `ingest[generic-git]: fetch failed, using local refs` | `git fetch origin <branch>` failed (credentials, network). Polling continues on local refs; `GIT_TERMINAL_PROMPT=0` prevents blocking |
| `evidence/` keeps growing | Prune runs hourly in `loop` only. Lower `retain_pass_days` / `retain_fail_days`, or delete old `runs/` and `agent/` dirs by hand; incidents are never pruned |
| `vigil validate` warns `duplicate fingerprint` | Two files are the same logical scenario; prefer a variant or delete one |
| `discover needs the Browser Agent` | `discover` runs the agent inline; fix the `doctor` agent checks first |
