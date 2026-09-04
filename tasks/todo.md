# Tasks

- [x] Add the shared request registration contract and user priority.
  - Acceptance: validated read-only input produces one manual feature and one top-priority Agent job.
  - Verify: orchestrator/store unit tests.
- [x] Add cooperative Agent preemption and recovery.
  - Acceptance: running Agent job returns to READY; its attempt and budget are not consumed; user job claims next.
  - Verify: scheduler concurrency test.
- [x] Add the main-page form, POST endpoint, and persisted request statuses.
  - Acceptance: one-field submission is accessible, bounded, and visibly trackable.
  - Verify: UI handler tests and browser runtime check.
- [x] Update ADR, README, and PRD.
  - Acceptance: priority, trust boundary, and interruption semantics are documented.
  - Verify: documentation diff review.
- [x] Run final quality gates and merge the worktree.
  - Acceptance: focused/full tests, vet, build, and browser proof pass.
