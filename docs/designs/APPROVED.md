# APPROVED — Paper Amber Console

**File:** `hybrid-1-paper-amber-console.html`
**Date:** 2026-06-17
**Status:** locked direction; production implementation pending.

## Brief

A personal AI operations cockpit for a Senior Software Engineer II (also operating as staff) in External Platform Engineering. Local web service (extends the existing `~/git/cockpit` Go server), with every entity mirrored to `~/cockpit-vault/` as Markdown for Obsidian archival.

The cockpit must serve four jobs:

1. Stay on top of RFC and PR reviews.
2. Stay on top of Slack conversations.
3. Stay on top of TODOs extracted from email, Slack, and Zoom meetings.
4. Track work outside the team across External Platform org (staff-radar lane).

Operating principle: **never miss a thing.**

## Primary surface

A single page with three vertical lanes plus a top gauge strip:

- **Left** — your workstreams (tree, expanded for the active one) stacked above the staff-radar lane (cross-org work, RFCs needing you, your asks-to-other-teams).
- **Centre** — workstream detail panel with overview / graph / timeline / obsidian tabs, AI-drafted next actions, a root→leaf SVG graph, and engineering / asks / signals tables.
- **Right** — inbound stream (PR / Slack / Jira / Zoom / LD / Gmail items) stacked above today's calendar tile (next meeting + free focus blocks).
- **Top** — six gauges (unprocessed · sla-risk · workstreams · staff radar · today's focus · pages 24h) and a per-source health-pill row (slack / gh / jira / dd / zoom / gmail / ld / cal).
- **Keymap strip** under the masthead: `j k`, `g w / g s / g i`, `e`, `s`, `a`, `.`, `?`.

## Key visual decisions

- **Surface:** warm paper cream (`#F5F1E8`), elevated cards on `#FBF8F1`, sunken regions on `#ECE6D6`.
- **Type:** Space Grotesk for chrome and headings, JetBrains Mono for data, IDs, ticket refs, and ASCII status codes.
- **Accent:** single amber (`#D88A2C`). No rainbow status. Severity surfaces through filled-amber dots and ASCII codes `[!]/[·]/[✓]` — never through alternative hues.
- **Density discipline:** every list row is single-line where possible; tables use mono and tight 8px row padding; hover state on every interactive row to remove the "is this clickable?" ambiguity.
- **Affordance:** real buttons with borders, hover backgrounds, and visible `kbd` shortcuts; nothing is ASCII-only.

## What's in scope

- Workstream tree + state propagation (red leaf → orange branch → workstream needs attention).
- Staff radar as a first-class lane on the same page (not a tab).
- AI-drafted next-action card per workstream, with selectable rows and time estimates.
- Root → leaf SVG graph view for the selected workstream.
- Today's calendar tile with focus blocks and next meeting.
- Per-source health pills (last-synced age, failure state).
- Keyboard-driven navigation across the page.
- Obsidian markdown mirror under `~/cockpit-vault/` (one file per workstream / leaf / ask / decision).

## What is NOT in scope

- **Dark theme.** Considered (Mission Amber) and dropped — light paper won. Defer until/unless a screen-share or eye-strain case warrants it.
- **Editorial / serif voice.** Considered (Slow Folio) and dropped — the cockpit is an operator surface, not a daybook. The serif drop-cap / margin-notes feel reads warm in the morning but slows scanning during the day.
- **Force-directed live graph view.** SVG static graph is in; an interactive d3-force version is a v2 idea, not v1.
- **Multi-user / team mode.** Single user, single machine, 127.0.0.1.
- **Auto-write actions.** Cockpit drafts; the user ships. Nothing posts to Slack / GitHub / Jira without explicit user confirmation.
- **Mobile native.** Page survives 375px viewport (stacked layout) but no native iOS / Android app.
- **Sound / animation.** No notification sounds, no animated transitions beyond a 120ms hover background fade.

## Score table (final)

```
VARIANT          | PICK | SCORE | KEPT IN HYBRID                                   | DROPPED
-----------------|------|-------|--------------------------------------------------|------------------------
Paper Terminal   | ◐    | 3.5/5 | density · ASCII status codes · single-accent     | "tacky" feel · unclear
                 |      |       |   restraint                                       |   what's clickable
Slow Folio       |      | —     | —                                                | overall direction
Mission Amber    | ●    | 4/5   | gauge strip · SVG root→leaf graph · per-source   | dark theme (light paper
                 |      |       |   health pills · "welcoming/organized" hierarchy  |   surface won)
```

## Implementation notes (for Step 6)

The existing `cockpit` Go server already provides the substrate: SQLite, Tailwind via CDN, server-rendered `html/template`. The hybrid is a single page; to ship it:

- Add new templates: `map.tmpl` (this page), `workstream_detail_panel.tmpl`, `staff_radar_panel.tmpl`, `stream_panel.tmpl`, `calendar_tile.tmpl`.
- Extend the layout template's font block to include Space Grotesk + JetBrains Mono from Google Fonts.
- Add new tables: `workstreams`, `leaves` (polymorphic), `asks`, `signals`, `staff_items`, `obsidian_mirror`.
- New scanners (matching the existing `scanner_*.go` pattern): `scanner_slack.go`, `scanner_gmail.go`, `scanner_zoom.go`, `scanner_jira.go`, `scanner_calendar.go`.
- Markdown mirror: `markdown_sync.go` writes/updates one file per entity under `~/cockpit-vault/{type}/{slug}.md` with YAML frontmatter on every save.

Build order is best decided in a separate planning pass — this document only locks the visual direction.
