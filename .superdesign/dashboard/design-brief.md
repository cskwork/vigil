# design-brief - the Read

- Reading this as: data-dense QA operations console for non-developer QA staff and managers, calm trust-first language, leaning the "data-dense business app / console" lane (dashboard.md overlay) implemented in plain CSS (single embedded HTML, no framework, no design-system package).
- Kind / medium: internal admin console (live status board) / web (single self-contained HTML served by the Go binary)
- Audience: non-developers (QA staff, managers) who need to answer "is the service OK, what is being checked, what is the AI doing, what needs my attention" without reading code; developers get a secondary "개발자 정보" toggle
- References / brand: none owned; Korean UI; existing IA to preserve (overview, now running, AI helper, scenarios, incidents, recent runs, deployed changes); sibling internal tools are plain enterprise back-office
- Quiet constraints (override aesthetics): WCAG AA contrast; status never by color alone (label + shape); reduced-motion honored; Korean word-break keep-all; may run on an intranet with no CDN (font must degrade to system Korean face); read-only surface, no destructive actions
- Sub-mode: REDESIGN / Overhaul visual language, keep content + IA + Korean copy voice
- Open question: none (non-interactive run; conservative read: preserve IA and copy, calmer dial row)
