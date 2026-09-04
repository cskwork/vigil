# PRD — vigil

**Version:** 0.4 — Final Lean Architecture  
**Status:** Implementation-ready  
**Purpose:** Portable continuous QA for already-deployed web applications

---

## 1. Product

`vigil` watches shipped Git features and continuously verifies the corresponding behavior on a real deployed URL.

Its efficiency model is hybrid:

> **Browser Agents discover or re-understand uncertain behavior. Deterministic QA scripts repeatedly verify proven behavior. The orchestrator decides which mode is justified.**

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

`vigil` is separate from the SDLC framework that produced the feature. `sdlc-kit` is an optional shipped-feature adapter, not a runtime dependency.

---

## 2. Goals and boundaries

### Goals

1. Detect newly shipped or changed Git features.
2. Test only after the target deployment is ready.
3. Reuse trusted scripts before launching expensive agent discovery.
4. Launch a Browser Agent subagent for new, uncovered, changed, or uncertain behavior.
5. Convert verified discoveries into deterministic reusable QA scripts.
6. Repeatedly execute those scripts without an LLM.
7. Run impacted old scenarios after later feature changes.
8. Prevent duplicate, flaky, obsolete, or low-value QA from growing forever.
9. Preserve reproducible evidence for confirmed regressions.
10. Scale from one laptop to tens of thousands of stored scenarios without changing QA contracts.

### Non-goals

`vigil` is not:

- a deployment system;
- a local app runtime;
- a unit-test framework;
- a load-testing platform;
- an APM product;
- an autonomous code fixer;
- a general crawler;
- a full visual-regression platform.

Normal QA uses an already-deployed URL. No target-app `npm install`, localhost server, Docker, VM, or local database is required.

---

## 3. Canonical rules

These rules are the single source of truth.

1. **The orchestrator chooses the QA mode.**
2. **New or uncertain behavior is checked agentically.**
3. **Proven repeated behavior is checked deterministically.**
4. **A Browser Agent discovery should end in a validated script when durable regression coverage is useful.**
5. **An agent may repair navigation/locators but may not silently change the business oracle.**
6. **Agent-generated scenarios begin as candidates, not permanent QA.**
7. **The scheduler is continuous; the full corpus is not.**
8. **Lightpanda is the default functional browser; Chromium is fallback/explicit fidelity verification.**
9. **A Git feature is not tested as deployed until deployment readiness is established or an explicit delay policy expires.**
10. **Mutable test accounts/data can be locked so concurrent scenarios do not corrupt one another.**
11. **One scenario/browser/worker/target failure never stops unrelated QA.**
12. **Existing deterministic QA continues even when the model/Browser Agent provider is unavailable.**

---

## 4. Roles and mode selection

| Role | Responsibility | LLM |
|---|---|---:|
| **Orchestrator** | Select mode, impact set, priority, retries and subagents | Not required for normal policy |
| **Browser Agent** | Explore deployed UI, verify change, generate/repair coverage | Yes |
| **QA Runner** | Execute stored scripts and assertions | No |
| **Lightpanda Worker** | Default functional browser execution | No |
| **Chromium Worker** | Confirm browser ambiguity or rendering-dependent behavior | No |

The orchestrator should be deterministic for common decisions. An agent may assist only when the decision itself is genuinely ambiguous.

### Decision table

| Condition | Action |
|---|---|
| New feature with no trustworthy coverage | `BROWSER_AGENT_DISCOVER` |
| Existing promoted script, routine regression | `RUN_SCRIPT` |
| New change with apparently relevant scripts | `RUN_IMPACTED_SCRIPTS_FIRST` |
| Existing scripts pass after change | Keep coverage; no discovery required |
| Change materially alters behavior or coverage is uncertain | `BROWSER_AGENT_VERIFY_CHANGE` |
| Script fails because locator/navigation drifted | `BROWSER_AGENT_REPAIR` |
| Script fails its business assertion | Classify; never auto-rewrite oracle |
| Agent finds unique durable behavior | `COMPILE_VALIDATE_PROMOTE` |
| Agent finds temporary exploratory behavior | `EPHEMERAL`; do not persist automatically |
| Lightpanda is unsupported/ambiguous | `CHROMIUM_CONFIRM` |
| Layout/rendering is part of the oracle | `CHROMIUM_VERIFY` |
| Target/auth/data is unhealthy | Classify infrastructure; suppress product incident |

Bias toward the cheapest trustworthy action:

```text
validated script
  → bounded script repair
  → Browser Agent verification/discovery
  → Chromium only when required
```

---

## 5. End-to-end flows

### 5.1 New feature → Browser Agent → fast script

```text
shipped FeatureEvent
      │
      ▼
Deployment Gate
      │
      ▼
Orchestrator
      │
trusted coverage missing
      ▼
Browser Agent subagent
      │
      ├─ read feature/spec/diff evidence
      ├─ open deployed URL
      ├─ authenticate
      ├─ exercise realistic user path
      ├─ inspect DOM
      ├─ inspect console
      ├─ inspect network/API behavior
      ├─ inspect popup/page state where relevant
      └─ verify expected business state
      │
      ▼
candidate scenario
      │
oracle provenance + evidence
      ▼
dedup/value gate
      │
      ▼
compile deterministic QA script
      │
      ▼
QA Runner executes script independently
      │
   ┌──┴───┐
 PASS    FAIL
  │        │
 SOAK   repair/review
  │
stable
  ▼
ACTIVE regression
```

Observation alone must not become the oracle.

Oracle evidence preference:

1. shipped specification/acceptance evidence;
2. previously approved QA oracle;
3. explicit business contract;
4. deployed observation as supporting evidence.

If correctness cannot be established:

```text
ORACLE_UNKNOWN
```

The check may remain exploratory but cannot become ordinary PASS/FAIL regression coverage.

---

### 5.2 Repeated QA

```text
scheduler
   ↓
load ACTIVE script
   ↓
acquire required test-data lock
   ↓
QA Runner
   ↓
Lightpanda
   ↓
actions + assertions
   ↓
PASS / classified FAIL
   ↓
persist compact result
   ↓
release lock
   ↓
schedule next due run
```

No LLM is required.

---

### 5.3 Changed feature with existing coverage

```text
new shipped SHA
      │
      ▼
Deployment Gate
      │
      ▼
impact selection
      │
      ▼
run relevant existing scripts
      │
 ┌────┴──────────────┐
 PASS                FAIL/uncertain
  │                        │
keep coverage              ▼
                    Browser Agent subagent
                       verifies change
                           │
                ┌──────────┴─────────┐
                ▼                    ▼
          app regression       valid behavior change
                │                    │
             incident          script revision
```

This is the default change path: **prove with existing scripts first; rediscover only when needed.**

---

### 5.4 Script drift

```text
script cannot locate/navigate
       │
       ▼
SCRIPT_DRIFT
       │
       ▼
Browser Agent
       │
 equivalent interaction exists?
       │
      yes
       ▼
propose locator/path patch
       │
same oracle still valid?
       │
      yes
       ▼
validate independently
       │
new script version
```

If oracle validity is uncertain, stop at `NEEDS_REVIEW`.

---

## 6. Deployment readiness gate

A Git "shipped" event can arrive before the deployed URL actually serves that revision.

Testing too early produces false failures.

Preferred readiness signals:

```text
release/version endpoint
deployment metadata
build SHA in page/API
external deployment event
```

Fallback:

```yaml
deployment:
  readiness:
    strategy: delay
    delay_after_ship: 2m
```

Possible states:

```text
WAITING_FOR_DEPLOYMENT
READY
DEPLOYMENT_UNKNOWN
```

If the expected SHA/version can be checked, do not execute feature-specific QA until it matches.

A deployment delay is not `APP_FAILURE`.

---

## 7. Feature ingestion

Core input:

```yaml
feature_id: remedy-page-order
status: shipped
shipped_sha: abc123
shipped_at: 2026-09-01T03:20:00Z
changed_paths:
  - src/pages/remedy/OctoPlayer.vue
routes:
  - /remedy
summary: Remedy display ordering changed.
```

Adapters:

```text
generic-git
sdlc-kit
future GitHub/GitLab/custom adapters
```

The core consumes normalized `FeatureEvent` records and does not depend on adapter internals.

Historical import:

```bash
vigil import --history
```

Historical onboarding is rate-limited so an old repository cannot create an immediate agent/discovery storm.

---

## 8. QA model

Keep the persistent model minimal:

```text
Project
 ├─ Feature
 ├─ Scenario
 │   ├─ Script Version
 │   └─ Variant
 ├─ Reusable Flow
 └─ Coverage Link
```

### Scenario

One logical:

```text
precondition
+ business action
+ expected result
```

### Reusable Flow

Shared behavior such as:

```text
login-as-student
select-course
open-current-assessment
```

### Variant

Parameters over the same logical scenario.

### Coverage Link

Connect scenarios to:

```text
feature
capability key
route
API
source path
persona
```

Relational joins are sufficient; no graph database is required.

---

## 9. Candidate, dedup and lifecycle

States:

```text
CANDIDATE
 ├─→ ACTIVE
 ├─→ DUPLICATE
 ├─→ EPHEMERAL
 ├─→ NEEDS_REVIEW
 └─→ REJECTED

ACTIVE
 ├─→ QUARANTINED
 ├─→ MERGED
 ├─→ SUPERSEDED
 └─→ RETIRED
```

A candidate becomes ACTIVE only if:

1. oracle provenance exists;
2. it executes successfully;
3. it adds durable coverage;
4. it is not duplicate;
5. mutation policy is acceptable;
6. its deterministic script validates independently.

### Fingerprint

Dedup inputs:

```text
business/capability goal
preconditions
major actions
oracle
route/API
persona class
```

Ignore locator mechanics.

These represent one logical scenario:

```text
click text="Submit"
click test-id="submit"
click role=button name="Submit"
```

Dedup order:

1. exact normalized fingerprint;
2. structural similarity;
3. agent semantic review only when necessary.

Prefer parameterized variants over copied scripts.

---

## 10. Deterministic QA script

Default QA scripts use a declarative DSL, not arbitrary generated code.

```yaml
scenario:
  id: visible-remedy-order
  version: 3

covers:
  feature: remedy-page-order
  capability: remedy.navigation

uses:
  - login-as-student

preconditions:
  persona: qa_student

steps:
  - goto: /remedy/current
  - assert_text: { value: "1" }
  - click: { by: role, role: button, name: Next }
  - assert_text: { value: "2" }

assert:
  no_uncaught_console_error: true
  no_http_5xx: true

oracle:
  source_feature: remedy-page-order
  source_sha: abc123
```

DSL advantages:

- deterministic;
- portable;
- schema-validatable;
- cheap to execute;
- easy to version/diff;
- safer than generated arbitrary code;
- independent of agent provider.

Add custom code only when a measured DSL limitation requires it.

### Locator order

```text
stable test id
→ accessible role/name
→ label
→ stable name/id
→ semantic text
→ stable href/action
→ CSS
→ Browser Agent repair
```

---

## 11. Browser Agent contract

The orchestrator gives a Browser Agent a bounded request.

```yaml
task: verify_changed_feature
feature_id: assessment-submit
shipped_sha: def456
target: https://dev.example.com
known_scripts:
  - assessment-submit/v4
changed_paths:
  - src/assessment/SubmitPanel.vue
allowed:
  - browse
  - inspect_dom
  - inspect_console
  - inspect_network
  - propose_scenario
  - propose_script_patch
forbidden:
  - change_oracle_without_provenance
```

Required result:

```yaml
decision: NEW_SCRIPT | PATCH_SCRIPT | APP_FAILURE | NO_NEW_COVERAGE | NEEDS_REVIEW
evidence: ...
coverage_delta: ...
oracle_provenance: ...
script_candidate: ...
```

Persistent state changes occur only after orchestrator validation.

Target navigation is restricted to configured/allowlisted domains.

Secrets must be redacted from agent output and evidence.

### Operator QA request

The main dashboard page has one text box for a non-developer to describe a QA
situation. The server accepts only the situation text, generates the feature
identifier, and forces the Browser Agent request to `read-only`. The UI cannot
set accounts, target URLs, mutation level, timeout, or tool budget.

The request enters the Agent queue at priority 110. If the same loop process is
running another Browser Agent job, the scheduler cancels that job cooperatively
and returns it to `READY` without consuming an attempt or Agent budget. It does
not cancel deterministic scenario workers. The displaced job resumes later.

The prioritized request may bypass `agent_tasks_per_hour` once so it can run
next when a Browser Agent is available. Its output remains a proposal. Existing
candidate validation, deduplication, oracle, and SOAK rules still apply.

---

## 12. Impact and scheduling

The scheduler runs continuously but selects a subset.

Impact signals, strongest first:

1. scenario explicitly covers changed feature;
2. same capability;
3. same route/API;
4. same changed source path;
5. explicit dependency;
6. historical co-failure;
7. agent-proposed relation.

Priority:

```text
operator QA request (110)
→ recent confirmed failure / repair (100)
→ new direct coverage
→ soak
→ impacted old coverage
→ P0
→ P1
→ P2/background
```

Selection considers:

```text
business risk
change impact
recent failures
time since last clean verification
coverage uniqueness
execution cost
```

Example defaults:

| Class | Cadence |
|---|---:|
| New direct coverage | immediate |
| SOAK | 10 min until confidence threshold |
| Recent failure | bounded retry/backoff |
| P0 | 15 min |
| P1 | 60 min |
| P2 | 6 h |
| Broad sweep | daily/weekly by corpus size |

Budgets prevent unlimited work:

```yaml
budget:
  browser_minutes_per_hour: 60
  chromium_minutes_per_hour: 5
  agent_tasks_per_hour: 10
```

Under pressure, defer background work rather than duplicating or dropping high-priority work.

An operator QA request gets one budget bypass for its next claim. This exception
does not disable the hourly budget for later Agent jobs.

---

## 13. Browser execution and classification

### Lightpanda

Default for functional QA:

```text
navigation
DOM/JS interaction
forms
cookies/session
console/runtime signals
network/API observation
supported popup/page flows
```

### Chromium

Use when:

```text
Lightpanda support is uncertain
browser/popup semantics are ambiguous
rendering/layout is the oracle
explicit cross-browser verification is required
```

A Lightpanda failure is never automatically an application defect.

### Outcomes

```text
PASS
APP_FAILURE
QA_FLAKE
SCRIPT_DRIFT
LIGHTPANDA_INCOMPATIBLE
BROWSER_AMBIGUOUS
AUTH_FAILURE
DATA_FAILURE
ENV_FAILURE
DEPLOYMENT_NOT_READY
ORACLE_UNKNOWN
NEEDS_REVIEW
```

Failure path:

```text
FAIL
 │
 ├─ cheap safe retry → PASS => QA_FLAKE
 │
 └─ classify
      ├─ script drift → Browser Agent repair
      ├─ browser ambiguity → Chromium
      ├─ auth/data/env/deployment → infrastructure result
      └─ business assertion still fails → APP_FAILURE
```

If unrelated critical scenarios fail together from DNS/TLS/gateway/auth-provider/target outage, create one environment incident instead of many feature incidents.

---

## 14. Authentication, data and concurrency safety

Secrets remain outside scripts.

```yaml
personas:
  qa_student:
    username: ${QA_STUDENT_USER}
    password: ${QA_STUDENT_PASSWORD}
```

Mutation classes:

```text
read-only
reversible
destructive
```

Default is `read-only`.

Production destructive testing requires explicit allowlisting.

### Resource locks

Scenarios that mutate shared data/accounts declare a lock key.

```yaml
resources:
  locks:
    - qa_student_01
    - assessment_fixture_A
```

The scheduler must not run conflicting mutable scenarios concurrently.

This prevents false failures caused by tests changing the same account/state.

Read-only scenarios need no lock unless the application itself requires one.

### Dashboard trust boundary

Dashboard controls are unauthenticated. Same-origin checks and submission rate
limits do not replace authentication. Bind `loop --ui` and `serve` only to a
trusted address. Standalone `serve` has no request submitter and rejects QA
submissions; `loop --ui` enables them only when the Browser Agent is available.

---

## 15. Evidence and incident output

Every run stores compact metadata:

```text
project
feature/scenario/script version
shipped SHA
browser
result
duration
attempt
timestamps
```

Failures additionally retain, when relevant:

```text
failed action
expected vs actual
DOM snapshot
console errors
failed requests
relevant response metadata
page/popup state
Chromium confirmation
```

Confirmed application regressions produce portable Markdown/JSON incidents suitable for an SDLC/coding agent.

`vigil` reports; it does not modify target code.

Retention should keep failure evidence longer than passing evidence.

---

## 16. Scalability

The same contracts run in two modes.

### Local

```text
single vigil process
SQLite WAL
local evidence
1–4 Lightpanda workers
Chromium on demand
```

Suitable for small/medium projects and thousands of selectively scheduled stored scenarios.

### Distributed

```text
PostgreSQL control state
object-store evidence
N Lightpanda workers
small Chromium pool
optional Browser Agent worker pool
```

Suitable for thousands/tens of thousands of promoted scenarios.

No broker is required initially.

PostgreSQL can hold the durable queue:

```sql
SELECT id
FROM qa_jobs
WHERE state = 'READY'
  AND scheduled_at <= NOW()
ORDER BY priority DESC, scheduled_at
FOR UPDATE SKIP LOCKED
LIMIT 1;
```

Workers use expiring leases and heartbeats. Dead-worker jobs return to the ready queue.

Large DOM/trace/screenshot artifacts live in local filesystem or S3-compatible storage, not normal relational rows.

Adding workers increases throughput without changing scripts.

---

## 17. Minimal persistent schema

Logical tables:

```text
projects
repositories
features

scenarios
scenario_versions
scenario_variants
flows
flow_versions
coverage_links

jobs
job_attempts
runs
run_artifacts
incidents

workers
scheduler_state
scenario_metrics
resource_locks
```

Key indexes:

```text
jobs(state, scheduled_at, priority)
jobs(lease_expires_at)
scenarios(project_id, state)
scenarios(fingerprint)
runs(scenario_id, started_at)
coverage_links(link_type, link_value)
features(project_id, latest_shipped_sha)
resource_locks(lock_key)
```

Do not add a graph database, broker, or separate service per policy concept without measured need.

---

## 18. Corpus health

Persistent QA must improve rather than only grow.

Track:

```text
runs
unique regressions caught
flake rate
median duration
last verification
coverage links
```

Periodically identify:

```text
duplicates
merge candidates
superseded scenarios
flaky/quarantined coverage
expensive low-value scenarios
coverage gaps
```

Do not automatically delete a scenario based only on age.

---

## 19. CLI and configuration

Core commands:

```bash
vigil init
vigil add <project>
vigil import --history
vigil scan
vigil discover <feature>
vigil run <feature-or-scenario>
vigil run --impacted <feature>
vigil loop
vigil status
vigil coverage
vigil incidents
vigil serve
vigil loop --ui 127.0.0.1:8787
vigil doctor
```

Example local config:

```yaml
version: 4

runtime:
  mode: local

project:
  id: student-web
  repo: /Users/me/src/student-web

target:
  base_url: https://dev.example.com
  allowed_hosts:
    - dev.example.com

discovery:
  adapter: sdlc-kit
  poll_interval: 60s

deployment:
  readiness:
    strategy: delay
    delay_after_ship: 2m

browser:
  primary: lightpanda
  fallback: chromium

workers:
  functional: 1

state:
  type: sqlite

evidence:
  type: local

budget:
  agent_tasks_per_hour: 10

policy:
  allow_destructive: false
```

Scale mode changes storage/workers, not QA contracts:

```yaml
runtime:
  mode: distributed

state:
  type: postgres
  dsn_env: QA_FLOW_POSTGRES_DSN

evidence:
  type: s3
  bucket: vigil-evidence

workers:
  functional: 20
  chromium: 2
```

---

## 20. Acceptance criteria

| ID | Requirement |
|---|---|
| AC-01 | Configure QA from Git repo + deployed URL + credentials without running the target locally. |
| AC-02 | Do not run feature QA before deployment readiness policy permits it. |
| AC-03 | New uncovered behavior launches a Browser Agent subagent. |
| AC-04 | Browser Agent can inspect deployed UI/DOM/network/console and propose coverage with oracle provenance. |
| AC-05 | Durable discovery compiles to a deterministic script and validates independently before ACTIVE promotion. |
| AC-06 | ACTIVE regression normally runs without an LLM. |
| AC-07 | Existing relevant scripts run before rediscovery after a change. |
| AC-08 | Orchestrator may launch Browser Agent verification when the changed behavior remains uncertain. |
| AC-09 | Agent repair cannot silently change the business oracle. |
| AC-10 | Duplicate generated scenarios cannot become duplicate permanent assets. |
| AC-11 | Reusable flows/variants reduce repeated full scripts. |
| AC-12 | New shipments trigger selected impacted old scenarios, not unconditional run-all. |
| AC-13 | Lightpanda is default; a clean Lightpanda pass does not launch Chromium. |
| AC-14 | Browser-ambiguous failures can be confirmed with Chromium before APP_FAILURE. |
| AC-15 | Auth/data/env/deployment failures are separated from product regressions. |
| AC-16 | Mutable scenarios with the same resource lock cannot execute concurrently. |
| AC-17 | One scenario/worker failure does not stop unrelated QA. |
| AC-18 | Confirmed regressions include expected/actual evidence, script version and shipped SHA. |
| AC-19 | Deterministic regression continues if the Browser Agent/model is unavailable. |
| AC-20 | Local SQLite and distributed PostgreSQL modes execute the same scripts. |
| AC-21 | Distributed workers use durable leases and recover work after worker death. |
| AC-22 | Tens of thousands of stored scenarios remain selectively queryable/schedulable without loading or executing the whole corpus. |
| AC-23 | Restart preserves feature, scenario, script, queue, result and incident state. |
| AC-24 | Continuous generation cannot bypass candidate/dedup/oracle/promotion gates. |
| AC-25 | The main dashboard accepts one bounded read-only QA situation and schedules it at priority 110, above priority-100 repair work. |
| AC-26 | A user request cooperatively requeues only the active Browser Agent job without charging its attempt or budget; deterministic workers continue. |
| AC-27 | A prioritized user request may bypass the hourly Agent budget once, but its candidates still pass the existing validation and SOAK lifecycle. |
| AC-28 | Dashboard controls require a trusted bind because they are unauthenticated; standalone `serve` rejects QA submissions. |

---

## 21. Implementation

### Phase 1 — lean local product

Build:

```text
Go binary
SQLite
Git watcher + generic/sdlc-kit adapters
Deployment Gate
Orchestrator
Browser Agent adapter contract
scenario/flow/coverage model
candidate + fingerprint/dedup
QA DSL + validator
Lightpanda runner
Chromium fallback
impact scheduler
resource locks
failure classification
evidence/incidents
CLI
```

Required closed loop:

```text
ship
→ wait until deployed
→ orchestrator selects script or Browser Agent
→ agent discovers new behavior when required
→ validated deterministic script is produced
→ future checks run script without LLM
→ impacted old scripts continue running
```

### Phase 2 — scale without redesign

Add:

```text
PostgreSQL
durable leases
multi-worker Lightpanda
small Chromium pool
S3-compatible evidence
worker heartbeat
project fairness/backpressure
optional Browser Agent worker pool
```

### Phase 3 — only after measured need

Consider:

```text
semantic dedup assistance
historical co-failure impact
automatic cadence suggestions
merge/retirement suggestions
advanced coverage analytics
```

Do **not** add Kafka/Redis/RabbitMQ, a graph DB, Kubernetes operator, browser cloud, or arbitrary-code test generation unless measured requirements justify them.

---

## 22. Definition of Done

`vigil` is successful when this loop can run unattended:

```text
Git feature ships
      │
      ▼
Deployment Gate
      │
      ▼
Orchestrator
      │
      ├─ trusted script → execute fast
      │
      └─ new/uncertain → Browser Agent
                            │
                     inspect deployed site
                            │
                     candidate + evidence
                            │
                       dedup/oracle gate
                            │
                       validated script
      ┌─────────────────────┘
      ▼
Lightpanda regression
      │
 PASS / classified FAIL
      │
 browser ambiguity
      ▼
 Chromium only if needed
      │
      ▼
health / incident / next schedule
      │
      └──────────────→ Orchestrator
```

The long-term system must retain these properties:

- new behavior can be discovered autonomously on the actual deployed application;
- repeated known QA becomes deterministic and fast;
- the orchestrator decides when a Browser Agent subagent is worth launching;
- scripts remain independent of the model provider;
- old functionality continues to be tested after new features ship;
- scenario growth is governed by deduplication, reuse and lifecycle;
- mutable QA does not conflict through shared accounts/data;
- deployment lag does not create false regressions;
- thousands of stored scenarios are selectively scheduled;
- scale is achieved by adding stateless browser workers, not by running the entire corpus continuously.

> **Discover with an agent when knowledge is missing. Compile that knowledge into a script. Reuse the script until evidence says rediscovery is justified.**
