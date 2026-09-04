# Capability Map: User-prioritized QA

| Module id | Responsibility | Depends on |
|---|---|---|
| manual-request | Validate and persist one plain-language, read-only QA request | — |
| agent-preemption | Requeue the running Browser Agent job and give the user request the next lease | manual-request |
| dashboard-intake | Accept the situation on the main page and show its queue/run result | manual-request, agent-preemption |

Build order: manual-request → agent-preemption → dashboard-intake
