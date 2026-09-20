# Cockpit Console Roadmap

This is a small index of the next three modules discussed for the approved Paper Amber Console, not a replacement for the full approved design. Existing PR review and Sessions capabilities remain available. Source feasibility findings inform later adapters, not a requirement to install new integrations for the first module.

| Order | Module | Status | Scope / dependencies |
|---|---|---|---|
| 1 | [Workstreams + Map Page](specs/workstreams-map.md) | done | Implemented and verified 2026-09-20 on `feat/workstreams-map` (`9690f50`; not merged). Local workstreams, tasks/references/asks/signals, attention, Map views, history, and Markdown mirror. No new provider dependency |
| 2 | Unified inbound stream | planned | Reuses workstream identities and reference attachment. Glean-first communication discovery and existing operational access; source completeness and triage contracts still need specification |
| 3 | AI-drafted next actions | planned | Reuses workstream context and inbound provenance. Draft/accept locally; external actions always require explicit confirmation. Not yet specified |

## Decisions so far

### Workstreams + Map Page

- Use the [approved console direction](designs/APPROVED.md) and [selected mockup](designs/hybrid-1-paper-amber-console.html).
- Make the first module usable with manual records and existing cached PRs. Preserve current review and Sessions workflows.
- Separate workstream lifecycle, local tracking state, derived attention, source freshness, and mirror health.
- Implement overview, static graph, timeline, and the one-way Markdown mirror. Show other planned console areas as unavailable, without fabricated data or active controls.
- Testing boundaries were agreed with the user: existing HTTP behavior tests with real temporary SQLite/vault storage; browser acceptance checks; no live provider dependency.
- Full implementation approved on 2026-09-20 using the `/implement` markdown-plan workflow; [execution and verification plan](plans/workstreams-map-implementation.md).

### Integration reuse and follow-on modules

- The September 5 and September 7 feasibility checks confirmed usable existing integration routes, including Glean source-specific searches for Gmail, Calendar, Slack, and Zoom.
- Source access remains distinct from full-content, freshness, and enumeration guarantees; those contracts need further validation in the inbound module.
- Prefer existing Glean/devtools/MCP access before building separate provider clients. No new source scanners or AI actions are included in the first module.
- Staff radar, calendar/focus, additional gauges, and other approved features remain later work; their relative order has not been settled by this index.
