# claims - designer self-audit (one entry per surface)

## vigil live board (internal/ui/index.html)

- Mode + medium: REDESIGN (overhaul visual language, keep IA + Korean copy) / web
- Design Read: data-dense QA operations console for non-developer QA staff and managers, calm trust-first, leaning the data-dense console lane (dashboard.md) in plain CSS
- Dials: DESIGN_VARIANCE 3 / MOTION_INTENSITY 2 / VISUAL_DENSITY 6
- Accent / type / radius / theme: accent #0f766e (teal, single); type Pretendard Variable with system Korean fallback, tabular numerals on every number; radius 10px single system; single light theme, off-white canvas #f4f5f7 / surface #fbfbfa, off-black text #1b1f24
- Assets: none required (no imagery on an ops console); status cues are CSS shapes + text labels, no icon library, no hand-rolled SVG
- Claim: app shell (text nav rail + breadcrumb topbar + global search + freshness tick + density toggle), KPI row with north-star top-left and baselines/targets, exception-first sections (now running, AI helper, checks, issues, history, deploys), per-widget skeleton/empty/error states with inline retry, status never by color alone, reduced-motion honored, keep-all Korean line breaks, no em dash, no emoji, no purple, no pure black/white
- Framings: 390x844 mobile, 1440x900 desktop
- run-to-prove: `bash ~/.claude/skills/superdesign/templates/preflight-gate.sh .superdesign/dashboard internal/ui/index.html internal/ui/scripts.html internal/ui/theme.css`

## scripts page (internal/ui/scripts.html, shared internal/ui/theme.css)

- Mode + medium: CREATE (sibling surface in the same shell) / web
- Design Read: same console read; the page answers "which fixed scripts exist, where did each come from, what exactly does it check"
- Dials: DESIGN_VARIANCE 3 / MOTION_INTENSITY 2 / VISUAL_DENSITY 6
- Accent / type / radius / theme: identical tokens via theme.css
- Assets: none
- Claim: list with origin/state filters + global search, detail panel with version history (who/why/when, click to view any version), coverage links, YAML source; NEEDS_REVIEW rows show the approve command; per-widget skeleton/empty/error
- Framings: 390x844 mobile, 1440x900 desktop
- run-to-prove: same command as above

## verification page (internal/ui/index.html) + report (internal/ui/report.html); old board moved to /activity

- Mode + medium: REDESIGN (IA reordered around QA questions: 검증 결과 → 검사 스크립트 → 시스템 활동) / web
- Design Read: same console read; home now answers "what was verified on this deployed build and where is the evidence"
- Dials: DESIGN_VARIANCE 3 / MOTION_INTENSITY 2 / VISUAL_DENSITY 6
- Accent / type / radius / theme: identical tokens via theme.css
- Assets: none; evidence screenshots are real run artifacts
- Claim: KPI row with north-star service status per deployment, matrix (check × latest result × evidence badges × time), row detail with tabs (steps / screenshots / server responses / console), deployment history, Markdown export, printable report page with per-check steps and screenshots; per-widget skeleton/empty/error
- Framings: 390x844 mobile, 1440x900 desktop
- run-to-prove: `bash ~/.claude/skills/superdesign/templates/preflight-gate.sh .superdesign/dashboard internal/ui/index.html internal/ui/activity.html internal/ui/scripts.html internal/ui/report.html internal/ui/theme.css`

## Render (critic)

- Tool: playwright-cli 0.1.15
- Surface: http://127.0.0.1:8799/ (verification), /report, /activity, /scripts (vigil serve, live data from the running loop)
- Framings: 1440x900 desktop, 390x844 mobile
- Screenshots: render/verify-desktop-1440x900.png, render/verify-mobile-390x844.png, render/report-desktop-1440x900.png, render/desktop-1440x900.png (activity), render/mobile-390x844.png (activity), render/scripts-desktop-1440x900.png, render/scripts-mobile-390x844.png
- Checks: no horizontal overflow at either framing; KPI row and app shell fit the first viewport; nav rail collapses to a wrapped top strip on mobile; tables hide secondary columns below 820px
- Console: clean (Total messages: 0, Errors: 0, Warnings: 0)
- render-to-prove: `bash ~/.claude/skills/superdesign/templates/render-gate.sh .superdesign/dashboard`
