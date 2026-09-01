# features/

Drop-in `FeatureEvent` YAML files for the `file` discovery adapter (PRD §7).
The default adapter for this project is `generic-git`; this directory is the
manual fallback (e.g. a shipped feature that is not visible in git history).

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

Each file is ingested once per `shipped_sha` (marker in `scheduler_state`).
