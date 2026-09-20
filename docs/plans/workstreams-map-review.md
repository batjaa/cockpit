# Workstreams + Map: implementation review

Reviewed against the approved [spec](../specs/workstreams-map.md), [execution plan](workstreams-map-implementation.md), and baseline `ee6b28ff1072e7e1b9e12dc9f951af13943f7a59`. GPT-5.6 Terra agents performed independent Bugs, Spec, Standards, and Design reviews; the primary agent integrated fixes and verification.

## Bugs

Open confirmed findings: **0** in the final targeted correctness/security recheck. Initial final pass: two findings, worst severity high; both fixed. Follow-up review found another broken-link case and a detached-source validation edge; both fixed.

- Enforce the parent revision when creating an item, including stale browser submissions. Explicit stale revisions are never refreshed by the server.
- Reject decisions whose related item belongs to another workstream or whose source does not match the item. Source-only decisions require attached local context; explicitly item-linked historical decisions may refer to a detached item.
- Keep decisions immutable when an item moves, but resolve the item's current parent for its navigation link.
- Additional verification fixes bind persistent operation receipts to the submitted command, order additive migration columns before dependent indexes, and prevent SQLite WAL snapshot-upgrade failures by reserving the write transaction before reading.
- Existing regression cases cover lifecycle guards, actual parent/export revisions, reference reattachment, semantic no-ops, source canonicalization, retained drafts, and bounded fields.

Focused regressions demonstrated the stale-create, cross-workstream relation, reused receipt, old-schema upgrade, and moved-link failures before their corresponding production fixes. The final recheck found no remaining confirmed issue in those corrected paths. This is not a guarantee that the application has no undiscovered bugs.

## Spec

Open findings: **0** after fixes. Three gaps were corrected: require a source for a context reference (use the required human title as the default source label), show sidebar attention counts, and expose decision-specific mirror status/location/error/retry controls.

The module includes local workstreams and their lifecycle; tasks, references, asks, and manual signals; computed attention and data warnings; cached PR selection and safe unfetched URL references; overview, static graph, timeline, and Markdown views; recoverable detach/move; immutable decisions; bounded paging/search; timezone-aware deadlines; durable one-way exports; explicit unavailable states for later integrations.

Verification spans HTTP commands and rendered responses using real temporary SQLite/vault storage, controlled clocks and DST boundaries, 100 workstreams with 10,000 items/history records, preserved pre-Map PR/session data across reopening, and real-browser user journeys with external requests blocked. Export failure tests exercise collisions, symlinks, external edits, failed roots, restart/backoff, old acknowledgements, repeated retry, and concurrent publication. No live provider is needed or changed.

## Standards

Open findings: **0**. Review checked the applicable project instructions and standards baseline. New UI uses Tailwind utilities, native controls, escaped server templates, and locally served assets. Existing PR/Sessions behavior is retained. No production dependency was added; the optional browser harness uses a separately installed Playwright module. README documents configuration, mirror behavior, and isolated acceptance setup. Formatting findings were corrected with `gofmt`.

## Design

Accepted findings: **1**; no blocking design finding. The shared four-kind `WorkstreamItemInput` and command dispatcher mix common and kind-specific fields, increasing the number of branches to revisit when adding kinds. Retain this bounded tradeoff for the four fixed kinds in the approved spec: normalization is centralized, form preparation is shared, and invalid/irrelevant fields are covered by behavior tests. If kinds expand, split kind-specific commands/validation behind the existing storage seam instead of continuing to grow the dispatcher.

The approved Paper Amber layout is retained with unavailable future panels rather than invented activity. Browser verification covers keyboard/focus, draft preservation, narrow screens, and extreme content. Boundary browser checks exposed overflow in long graph labels and export-list titles; fixes and the final evidence are recorded in the execution plan.

## Workflow adaptations and limits

- The complete module was explicitly requested. The parallel T1/T2/T3 ownership lanes and T4 integration gate are bundled into a reviewed feature checkpoint because their contracts depend on one another; this is an explicit adaptation of the markdown mini-ticket workflow, not separate tracker tickets silently batched.
- Before the feature commit, reviewers inspected the staged baseline diff plus subsequent working-tree fixes, rather than the docs-only HEAD diff. The final checkpoint contains the reviewed implementation and verification fixes. No merge is performed.
- No built-in code-review tool or `/simplify` skill was available. A dedicated Terra bug reviewer substituted for the former; the primary agent's consolidation and the independent Standards/Design reviews supplied the reuse, quality, and efficiency checks for the latter. These tools were not claimed to have run.
- The portable filesystem mirror detects ordinary external edits and avoids intentional overwrites. It is not a compare-and-swap sandbox against a hostile process with the same filesystem privileges. SQLite remains authoritative; no bidirectional sync or force-overwrite control is provided.
- Integration inventory and inbound/AI adapters remain separate planned work. Background preview uses only isolated fixtures, not the real user's database or vault.

Final commands, browser evidence, and completion status are maintained in the [verification record](workstreams-map-implementation.md#verification-record).
