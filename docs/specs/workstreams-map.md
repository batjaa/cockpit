# Spec: Workstreams + Map Page

Status: specced; engineering review pending; not approved for implementation.

## Problem Statement

An initiative lives across tickets, PRs, conversations, cross-team requests, operational signals, and personal notes. I can find individual records, but cannot easily answer what outcome they support, what needs me, what I am waiting on, or why the overall initiative needs attention. A merged PR is not necessarily a finished initiative, and a quiet source is not necessarily a healthy one.

Cockpit already stores PR/review data and agent-session metadata. It does not yet provide an outcome-oriented place to organize that work. Its first workstream module must be useful without waiting for additional integrations or AI classification.

## Solution

Add a Map page to the existing local Cockpit application. A workstream represents one outcome I am pursuing or tracking. I create it, link existing records or URLs, record tasks and cross-team asks, and capture manual signals and decisions. A tree and selected-workstream detail panel show the same underlying information, with explicit reasons for attention states.

Use the approved Paper Amber Console direction: warm paper, amber emphasis, compact tables, a persistent workstream list, overview/graph/timeline views, and a Markdown mirror for Obsidian. This module implements the workstream portion of that console. The future inbound stream, staff-radar lane, AI-drafted next actions, and calendar tile have honest unavailable states rather than sample data presented as live information.

The initial experience is manual-first and local-first. Linking a PR already in Cockpit reuses its cached information. Pasting another source URL records a reference without fetching it. No Glean, devtools, Slack, Google, Zoom, or model access is required to use or test this module.

## User Stories

1. As an engineer, I want to create a workstream with a name and desired outcome, so that I organize work around a result rather than an application.
2. As an engineer, I want optional owner, sponsor, and target-date fields, so that I can record context without a directory integration.
3. As an engineer, I want to edit a workstream without breaking its links or mirrored notes, so that its name and scope can evolve.
4. As an engineer, I want to see my active workstreams and their attention reasons, so that I can choose where to focus.
5. As an engineer, I want to search by workstream, item, person, or ticket reference, so that I can find an initiative from whichever detail I remember.
6. As an engineer, I want a bookmarkable selected workstream and view, so that returning to the page restores useful context.
7. As an engineer, I want to add a local task with an optional due date, so that obligations not represented in a tracker are still visible.
8. As an engineer, I want to attach an existing cached PR, so that its identity, source state, and review context are available without copying them manually.
9. As an engineer, I want to attach an RFC, ticket, Slack thread, email, or meeting URL with a label, so that related evidence stays together before its connector exists.
10. As an engineer, I want to distinguish a reference from an obligation, so that merely saving a document does not create unfinished work.
11. As an engineer, I want duplicate attachments detected within a workstream, so that retries and repeated pastes do not inflate its contents.
12. As an engineer, I want to reuse a source reference in multiple workstreams while keeping their local obligations independent, so that shared dependencies retain context.
13. As an engineer, I want to move or remove a mistaken attachment without deleting the source record, so that organizing work is recoverable.
14. As an engineer, I want to record a task's progress and explicit blocker, so that attention has a concrete explanation.
15. As an engineer, I want to record who I am waiting on, the request, and the next follow-up date, so that cross-team dependencies do not disappear.
16. As an engineer, I want to record a response or move the follow-up date, so that an ask reflects the latest agreement.
17. As an engineer, I want to resolve or cancel an ask explicitly, so that reading it or recording contact does not falsely mark it finished.
18. As an engineer, I want to record a signal with its source, observation time, and interpretation, so that a rollout or operational concern can inform the workstream before automated scanning exists.
19. As an engineer, I want old or missing observations labelled as such, so that stale information is not presented as a current assessment.
20. As an engineer, I want attention to propagate from an item to its category and workstream, so that a compact tree still reveals unresolved problems.
21. As an engineer, I want to inspect the exact items causing attention, so that an aggregate badge is actionable.
22. As an engineer, I want the overview and graph to lead to the same item details, so that switching views does not change the meaning of the workstream.
23. As an engineer, I want a chronological history of local changes and decisions, so that I can recover why a workstream changed direction.
24. As an engineer, I want to record an outcome and explicitly complete a workstream, so that completion represents my judgment rather than a percentage calculation.
25. As an engineer, I want to reopen, archive, and restore a workstream, so that changing circumstances do not require rebuilding its history.
26. As an engineer, I want every workstream, engineering item, ask, signal, and decision mirrored as readable Markdown, so that my archive is usable outside Cockpit.
27. As an engineer, I want mirror errors and conflicts shown separately from successful saves, so that I know whether my archive is current without losing application data.
28. As an engineer, I want keyboard navigation, visible focus, and usable narrow-screen layouts, so that the Map works without a mouse or a large monitor.
29. As an engineer, I want missing integrations and future panels clearly labelled, so that I do not mistake absence of data for absence of obligations.
30. As an existing Cockpit user, I want PR reviews and Sessions to keep working unchanged, so that the new page does not interrupt today's workflow.

## Implementation Decisions

### 1. Module boundary and existing application

- Extend the existing single-user Go service, server-rendered HTML, SQLite persistence, and vendored Tailwind setup. Do not add a separate frontend application, external database, or generic workflow engine.
- Add Map as a peer navigation destination. Keep the current PR dashboard as the default landing page and preserve existing PR and Sessions URLs and behavior.
- The Map has its own full-width Paper Amber presentation. Do not restyle the existing review and Sessions pages as part of this module.
- Domain operations own validation, state transitions, history recording, and mirror scheduling. HTTP handlers translate requests and responses; templates render prepared view data.
- Workstream reads use local data only. A GET must not fetch URLs, invoke a CLI/model, start a scan, write the vault, or mutate workstream history.
- Existing PR discovery/reconciliation remains responsible for source updates. This module does not expand which repositories or PRs Cockpit discovers.

### 2. Domain vocabulary and persistence

Use stable opaque IDs for all new entities. Titles and filenames are not identities. Persist fixed states as validated enums with database constraints; reject unsupported states rather than silently mapping them to a default.

| Entity | Meaning and minimum information |
|---|---|
| Workstream | One desired outcome; name, outcome, owner label defaulting to me, optional sponsor/target date/notes, lifecycle, archive metadata, revision, timestamps |
| Engineering item / leaf | A local task or a linked reference belonging to one workstream; title, kind, optional description/source reference/due date, local tracking state where applicable, blocker reason, revision, timestamps |
| Source reference | Source kind, safe URL and canonical identity; optional foreign key to an existing cached PR. Source metadata and local task state are distinct |
| Ask | One request to another person/team; request text, counterpart label, optional source reference, status, optional next-follow-up time, latest recorded contact time, revision, timestamps |
| Signal | One manually recorded observation; label, optional value/unit/source URL, observation time, explicit assessment and optional review-by time, revision, timestamps |
| Decision | An append-only decision/outcome note attached to the workstream, with optional related item and source references |
| Timeline event | An append-only local change record with entity identity, event type, timestamp, revision, and a bounded readable description |
| Mirror state | Per-entity desired revision, last successfully written revision/checksum, attempt/success times, and pending/synced/error/conflict status |

The tree has exactly three category branches: Engineering, Asks, and Signals. These are projections of the entities above, not user-created folders. Arbitrary nested workstreams, dependency DAGs, and configurable categories are deferred. Decisions appear in the overview/timeline and mirror, not as a fourth task branch.

All new tables are additive. Preserve existing PRs, reviews, comments, sessions, and scan state. Enforce foreign keys and indexes for parent references, canonical identity lookups, list filters, due/follow-up queries, history pagination, and pending mirror work. Upgrading an existing database is idempotent and retains all existing data.

Field limits must be shared by forms and backend validation: names/titles/person labels up to 200 characters, outcome up to 4,000, descriptions/notes up to 32,000, and source URLs up to 4,096. Name and outcome are required for a workstream; whitespace-only required fields are invalid. Dates are optional, not silently guessed.

### 3. Workstream and item lifecycle

- Workstreams have an explicit lifecycle of active or completed, plus an independent archived flag. Archiving preserves the lifecycle, all children, history, and files. Restoring clears the archived flag without inventing a new lifecycle.
- Active is the default. Creation requires only name and desired outcome. Owner/sponsor fields are labels, not authorization rules or user accounts.
- Completion always requires a confirmation and an outcome note. If unresolved tasks/asks or concerning signals remain, list them and require a reason for completing anyway. Do not silently resolve, cancel, or delete them.
- Reopening is explicit and recorded in the timeline. Completed or archived workstreams are read-only until reopened or restored as appropriate; recording a reopening/restore event remains possible.
- Archiving an active workstream warns that its open obligations will leave the default active view. Archived records remain searchable through an explicit filter.
- Local tasks use open, in-progress, blocked, done, or cancelled. The first three are unfinished; done/cancelled tasks contribute no due-date or blocker attention. Blocked requires a reason. Leaving blocked clears the current blocker while preserving the old reason in history.
- Linked references are context, not obligations: they have no task completion requirement. A user can add a task linked to that reference, such as “review this PR” or “merge this change.”
- Asks use open, waiting, resolved, or cancelled. Open means I still need to make/advance the request; waiting means I have recorded that I am waiting on the counterpart. Recording contact does not resolve the ask or automatically postpone its follow-up date.
- Signals use an explicit manual assessment of normal, concerning, or unknown. There are no computed metric thresholds or inferred rollout verdicts in this module. Every signal is visibly labelled manual and includes when it was observed.
- Removing an item is a recoverable detach, not deletion of its source or history. Detached entities are excluded from the current tree but retained in the timeline and archive. Reattaching the same context reference restores it by canonical identity; tasks, asks, and signals are explicitly restored by their entity IDs, not merged by similar text or URLs.
- Moving an item between active workstreams is one atomic operation. History records the move in both workstreams and its mirror reflects the new parent. If moving a context reference would duplicate a reference already in the destination, reject the move and offer to open that reference. Distinct tasks may share a source; do not merge or reject them merely because their source URLs match.

### 4. Attention is separate from completion and freshness

The workstream and its category branches derive attention from active, attached contents; users do not directly edit the aggregate badge.

| Condition | Effect on an active workstream |
|---|---|
| Blocked task, overdue unfinished task, overdue unresolved ask, or concerning signal | Needs attention; retain every contributing reason |
| Past workstream target date | Needs attention with a target-date reason |
| Open task not overdue or an open ask not yet sent | Show under “Needs me”; does not by itself imply a blocker or overdue alert |
| Waiting ask with no overdue follow-up | Waiting when there are no higher-priority attention reasons |
| None of the above | No recorded attention items, not a claim that all external work is healthy |

Precedence is needs-attention, then waiting, then no-recorded-attention. Branch badges aggregate their own children; the root aggregates all branches and its target-date reason. Counts are counts of distinct entities, even if an entity has several reasons.

Missing metadata, unknown signal assessments, expired signal review-by times, and source freshness limitations appear as separate data-quality warnings. An old concerning signal remains concerning and also becomes stale; age must not clear an unresolved concern. Mirror health is another independent status, not a workstream lifecycle or task state.

Due dates are date-only deadlines in the configured IANA timezone, defaulting to the machine timezone. A task/target date becomes overdue at the start of the following local day. Follow-up and signal review-by times are stored as UTC instants and rendered locally; they are due when the current time reaches the recorded instant. Past dates are allowed and immediately explained as overdue. Missing dates mean unscheduled, not overdue. UI, derived state, and tests use one consistent notion of time.

Reading, selecting, expanding, or opening an item never changes its status. General inbox snoozing is deferred. For an ask, “Follow up later” explicitly edits its next-follow-up time and records the change; it does not cancel or resolve the request.

Completed and archived workstreams keep their declared lifecycle. Their details retain unresolved reasons and warnings, but they are excluded from the default active attention queue. No background observation automatically reopens them.

### 5. Linking source records without new integrations

- Provide two entry paths: choose a PR already stored in Cockpit, or enter a labelled source URL. Both require an explicit choice of destination workstream and whether the user is adding context or a local task.
- The PR picker searches local records, is paginated, and shows repository-qualified identity and current cached source state. If a pasted GitHub PR URL matches a local record, reuse that identity. If it does not, save a plain reference and label metadata unavailable; do not invoke GitHub discovery.
- Cached PR title/state/last-observed time are read-only source fields. The Map can open the existing local review page when available and the original source URL. No source change is made by linking or editing local context.
- A PR merge or closure does not automatically complete a local task: reviewing, merging, and shipping are different obligations. Display the cached source state alongside local tracking state rather than conflating them. The timeline in this module is a local action history, not a comprehensive external-event feed.
- Canonical PR identity includes host, repository owner/name, and PR number. Generic links normalize scheme/host/default port but preserve path, query, and fragment when their semantics are unknown. Do not strip thread/message anchors or tracking-looking parameters indiscriminately.
- A workstream cannot have duplicate context references to the same canonical source. Repeated attach requests return the existing reference. Multiple explicitly distinct tasks may reference the same source. Duplicate source references across different workstreams are allowed.
- Store human-entered labels and source provenance without credentials or provider transcript dumps. Pasted URLs must be HTTP(S), contain a host, and contain no embedded username/password. Never fetch them to generate a preview. Browser links must be clearly labelled as external and prevent opener access.
- Show source state as “cached” with its last-observed time. Missing timestamps mean unknown freshness. Never display a connected or recently-synced provider pill merely because the integration PoC passed or a URL was attached.

### 6. Map page and interactions

The desktop layout retains the approved three-lane hierarchy, with the centre receiving most of the width. At narrow widths, stack the workstream selector, selected detail, then secondary/context panels. The centre must remain usable when every deferred panel is unavailable.

**Workstream list:** active by default, with explicit completed/archived filters and search. Show outcome name, owner, target date where set, and an attention badge/reason count. Order active workstreams by attention precedence, then earliest relevant deadline, then name and stable ID. Expansion and selection do not reorder the list mid-keystroke; apply changed ordering on the next explicit refresh/navigation.

**Creation and editing:** use a labelled form with inline validation, preserved input after errors, and an explicit Save/Cancel. Successful creation selects the new workstream. Cancelling creates nothing. A stale revision conflict preserves the user's input and offers reload/review rather than overwriting a newer edit.

**Overview:** show outcome and metadata, counts of open local tasks and unresolved asks, independent source/mirror warnings, attention reasons linking to affected rows, and Engineering/Asks/Signals sections. Include explicit Add task, Attach reference, Add ask, Record signal, and Record decision actions. Do not show a generic progress percentage; task counts and a manually recorded rollout percentage are different things.

**Item detail:** selecting a row opens its full local context and safe source links without losing the selected workstream. Actions reflect entity kind. Editing a reference cannot mutate the cached PR. Removing/moving requires clear destination or detach confirmation. After save, relevant counts and statuses update from the same committed data.

**Graph:** render a deterministic static SVG root → category → item tree. Nodes select the same details as list rows. It is a relationship view, not a separate editor. Include accessible names and an equivalent ordered text/list representation. No force layout, drag-to-reparent, graph physics, or canvas-only interaction.

**Timeline:** show append-only creation, edits, attachment/detachment/moves, tracking-state changes, follow-up changes, recorded observations, decisions, lifecycle changes, and mirror conflict/recovery summaries. Record semantic changes only; repeated no-op saves or mirror retries must not flood history. Sort newest first with a stable event-ID tie-breaker. Source timestamps remain distinct from local event timestamps.

**Obsidian:** show the generated Markdown preview and per-entity/workstream mirror status, with a user-initiated open/copy-location affordance. A missing Obsidian app must not block access to the Markdown or the Map. Obsidian navigation is not a server endpoint that opens arbitrary local paths.

**Deferred surfaces:** reserve the staff-radar, inbound/calendar, and drafted-action positions with concise “Not available in this module” explanations. Hide inactive action controls and shortcuts. Only the active-workstream gauge is populated in this module; unprocessed, SLA-risk, staff-radar, focus, and paging gauges show unavailable rather than zero. Source pills show unavailable/not connected unless backed by actual runtime status. The mirror pill uses real local mirror state.

### 7. Navigation, scale, and presentation

- Preserve selected workstream, tab, item, search, and list filter in navigable URL state. Back/forward and a copied link reproduce the view. A missing entity produces a clear not-found state; an existing archived entity can be opened directly with an archived banner.
- Use native controls and semantic headings. Implement the relevant approved shortcuts: j/k to move among visible rows, g then w for workstreams, e to open the selected item, slash for search, and question mark for shortcut help. Enter/Space activate controls and Escape dismisses overlays and restores focus. Do not intercept typing in inputs, textareas, editable content, or IME composition. Deferred staff/inbox/drafting shortcuts are omitted.
- Keep amber as the only status accent. Severity is also expressed through readable labels and the approved ASCII indicators. Do not copy the mockup's standalone custom CSS into production; use Tailwind utilities and the approved font choices with local fallback stacks.
- Verify layouts at 375px, 768px, and desktop widths. Long names, URLs, and notes cannot overlap controls; truncated rows expose their full content in detail. Horizontal overflow, where unavoidable in the graph/table, stays inside that region rather than expanding the page.
- Empty, filtered-empty, loading, missing-record, validation, save-error, stale-edit, missing-source, and mirror-error states have distinct messages and relevant next actions. The initial empty state offers Create workstream, not fabricated examples.
- Database-bound lists use stable pagination rather than loading every row. Workstream and item lists use 50-row pages, timeline uses 50-event pages. Category totals and attention rollups cover all attached rows, not only the visible page. The graph includes at most 100 items at once and visibly identifies omitted items with a path to the full lists. Search includes child titles, counterpart labels, and known source identifiers across the locally stored dataset.
- The Map remains responsive for a local dataset of 100 workstreams and 10,000 attached items/history records. Verify bounded database reads and no per-row source requests; do not promise a latency percentile before measuring the implementation.

### 8. Local command/read contracts and consistency

The UI exposes read operations for listing/searching workstreams, reading a selected view, paginating children/history, searching cached PRs, and inspecting mirror state. Mutations cover workstream create/edit/complete/reopen/archive/restore; task/reference/ask/signal create/edit/move/detach/restore; decision recording; and explicit mirror retry.

Reads return prepared views containing lifecycle, attention reasons/counts, independent data warnings, and mirror status. Mutations validate parent and entity kinds, require the expected revision for existing records, and either commit the requested local change or return a clear conflict/validation failure with no partial effect. Creation/attachment forms carry an operation identity so retrying the same submission cannot duplicate records or timeline events.

Each successful semantic mutation commits entity changes, the timeline event, and desired mirror revisions in one SQLite transaction. Moving an entity also updates both workstream summaries. Do not write files inside that transaction. Referential failures, inactive-parent edits, invalid transitions, and stale revisions have explicit failure responses. An idempotent retry does not create another event or revision.

Use parameterized database operations, HTML escaping, safe Markdown rendering, request-body limits, and same-origin protections for new mutations. Localhost is not authorization to accept cross-site writes. Do not expose filesystem paths or SQL/credential details in browser errors, and do not add a remote/public hosting mode.

### 9. Markdown mirror contract

- SQLite is authoritative. The mirror is a one-way archive/export, not a second editable database. Explain that editing generated content in Obsidian does not update Cockpit.
- Default to the approved cockpit-vault directory under the user's home; allow an explicit alternate root or disabling the mirror. Missing configuration enables the approved default. Invalid/inaccessible vault configuration is a visible mirror error, not a reason to prevent local workstream saves.
- Write one file per workstream, engineering item, ask, signal, and decision, grouped by entity kind. Use stable IDs in filenames so renames and moves do not orphan files. Include YAML frontmatter with entity ID/type, parent ID where relevant, lifecycle/tracking state where relevant, revision, and timestamps. Include readable content and relative links to related mirrored entities. Workstream files summarize attached contents and link to decisions; signals and sources are labelled manual/cached with observation times.
- Export committed local fields, recorded dates, and explicitly dated source snapshots. Do not encode a moving relative-age counter or present a derived attention badge as a continuously current fact in Markdown. Time passing or a cached PR changing does not promise a fresh archive without a subsequent export; the file's revision and snapshot times make that boundary visible.
- Detached, completed, and archived entities remain in the mirror with their changed state. This module never recursively deletes a vault or removes unrelated notes.
- A bounded single-writer worker processes durable pending revisions after commit and resumes on startup. Write atomically using a temporary sibling file and rename. Rapid edits coalesce to the latest desired revision; an older write may never mark a newer revision synced. Parent summaries and both sides of a move are scheduled alongside the changed child.
- With a writable local filesystem, attempt pending writes within five seconds of the save. Failures retain pending work and last successful revision. Retry with bounded backoff, no faster than five seconds and no slower than sixty seconds, plus an explicit user retry. Shutdown/restart must not lose pending exports.
- “Saved in Cockpit” and “Synced to vault” are separate outcomes. Show pending counts, last success, and actionable error/conflict state. Failed mirrors do not roll back a saved task or completion and do not show a false synced timestamp.
- Mirror conflict/recovery transitions produce at most one timeline summary per operational state change. They do not increment content revisions or recursively enqueue new exports of themselves.
- Before replacing an existing generated file, compare its current checksum with the last written checksum. If externally changed, or if an unexpected existing file occupies the target, preserve it and report a conflict. Retry alone never grants overwrite permission. For this module, recovery is explicit user reconciliation/restoration or moving the conflicting file aside, then retry; no automatic merge or force-overwrite UI.
- Validate the configured root and managed descendant paths. Entity text cannot supply file paths. Reject traversal and symlink-based escapes; never overwrite a symlink target. Create only the explicitly configured managed directories and files. Filesystem safety and collision behavior are part of acceptance, not best-effort cleanup.

### 10. Representative acceptance walkthrough

Create “Credits v1” with an outcome and target date. Attach one cached PR as a reference, add a separate review task, record a Billing ask as waiting, and add a manual latency signal marked concerning. The Engineering branch shows the reference/task distinction; Asks shows waiting; Signals and the workstream show needs-attention with a direct link to the latency observation.

Advance the controlled clock past the Billing follow-up time. The ask adds an overdue reason without a database edit or external poll. Resolve the ask and mark the task done: the signal still keeps the workstream in needs-attention. A later normal observation clears that signal reason while preserving its history. Completion remains a separate confirmed action with an outcome note.

Reopen and then rename the workstream, detach/restore a reference, and reload the app. Identities, history, bookmarked views, and relative mirror links still work. Make the vault unwritable, save a local change, and observe saved-with-mirror-error; restore access and retry to export the latest revision without duplicates. At no point is an external application changed.

## Testing Decisions

The user agreed to these testing boundaries before this spec was written. Keep the implementation tests at these boundaries rather than building separate mock-heavy suites for every internal layer.

### Primary automated boundary: HTTP behavior with real local storage

Reuse the application's existing HTTP-handler test style with a real temporary SQLite database and real temporary vault directories. Exercise public local commands and reads, inspect rendered behavior and persisted outcomes, and reopen storage to verify durability. Inject controlled time and a deterministic way to drain scheduled mirror work; do not use sleeps or a live account to establish correctness. These controls support the same behavioral boundary rather than creating a second testing API for domain internals.

Cover:

1. Empty-state onboarding; minimal/full creation; validation; edit; repeat-submit idempotency; stable selection and identity after rename.
2. Add/edit/detach/restore/move for each entity kind; invalid-parent and destination-duplicate handling; cancelled forms leave no records.
3. Existing cached PR lookup, unavailable PR metadata, repository-qualified identity, task/reference distinction, and no source mutation or CLI invocation.
4. Local task/ask transitions, mandatory blocker explanations, follow-up dates, completion override confirmation/reason, reopening, and archive/restore semantics.
5. Every attention rule and its precedence; multiple reasons on one entity; categories/roots with several independent concerns; reading does not resolve work.
6. Date-only deadlines, exact follow-up boundaries, missing dates, timezone/daylight-saving boundaries, stale/unknown signal warnings, and no false healthy status.
7. Timeline ordering, semantic/no-op behavior, moves visible in both parents, immutable decision history, and persistence after restarting.
8. Filtering, child-aware search, stable pagination, totals beyond page one, a partially displayed graph, and no missing attention on hidden pages.
9. Concurrent/stale revisions, transaction rollback, repeated requests, missing entities, and errors preserving user input.
10. Markdown contents/frontmatter/relative links, stable filenames, rename/move/detach/archive behavior, pending-to-synced transitions, coalesced revisions, and restart recovery.
11. A genuine filesystem failure, an externally edited file, an unexpected path collision, and symlink/traversal rejection. Use deterministic filesystem fixtures rather than relying solely on permission bits, which privileged environments may ignore.
12. Saved-but-not-mirrored behavior and recovery; older exports cannot acknowledge newer revisions; repeated retry creates no duplicate history.
13. Cross-site mutation rejection, unsafe URL rejection, escaping of titles/notes/source labels, and bounded inputs. Assert no access to private real vaults or provider credentials.
14. Migration from existing Cockpit data, idempotent database reopening, and regression coverage for the current PR dashboard and Sessions.

### Browser acceptance boundary

Use the actual rendered app with local fixtures to check creation/edit forms, tree selection/expansion, item drill-down, navigation/back/forward, overview/graph/timeline consistency, and mirror status. Check keyboard-only use, focus restoration, shortcut suppression while typing, readable labels, long-content layout, and the three agreed viewport sizes. Include empty, partial, error, and stale-edit states; confirm unavailable panels have no working-looking controls.

These browser checks are required acceptance evidence. The implementation review can choose the runner based on available tooling; do not introduce a new browser framework merely to add a second assertion of every HTTP test. Do not assert generated script strings as a substitute for exercising keyboard or navigation behavior.

### Definition of done

The representative walkthrough works with all external tools unavailable. The agreed automated tests and existing regression suite pass; browser checks have recorded outcomes; no source data or unrelated vault files are changed. The implementation must not claim source ingestion, full inbox coverage, live signal collection, or AI drafting because the page renders their future positions.

## Out of Scope

- Unified inbound collection, Glean/devtools/MCP adapters, OAuth setup, automatic related-item discovery, and connector backfills.
- AI TODO extraction, suggested workstream assignment, drafted next actions/replies, estimation, or model execution.
- Posting or modifying anything in Slack, GitHub, Jira, Gmail, Calendar, Zoom, LaunchDarkly, Datadog, or other providers.
- Live rollout/metric evaluation, automated signal thresholds, incident counts, SLA policies, and comprehensive provider health monitoring.
- Staff-radar ingestion/ranking, a dedicated cross-org workflow, calendar/free-busy calculation, or scheduled focus blocks. A manually created workstream may still represent cross-org work.
- General inbox snoozing/assignment, full-text transcript storage, and a universal source-event model.
- Arbitrary nested workstreams, dependencies between workstreams, force-directed graphs, drag-and-drop hierarchy editing, and custom categories.
- Automatic external-event timeline reconstruction or automatic task completion from source status.
- Bidirectional Obsidian editing, generated-file conflict merging, destructive vault cleanup, or automatic relocation of an existing vault.
- Multi-user collaboration, cloud sync, remote hosting, dark mode, mobile-native clients, and redesigning existing PR/Sessions surfaces.

## Further Notes

The approved Paper Amber Console establishes the visual direction; the workstream interaction examples establish the user intent. This spec narrows the first module to useful local organization, explicit state, the Map views, and archival. Implementation-level structure and exact route naming belong in engineering review, not in this product contract.

The integration feasibility checks established Glean search access to Gmail, Calendar, Slack, and Zoom and existing routes for operational sources. That supports future adapters but does not make integration availability a prerequisite for this module. Later modules should reuse the workstream identity, local commands, reference identities, and explicit provenance; they must not silently reinterpret local tracking state.

Operational choices made explicit here include a fixed three-branch tree, separate reference/task semantics, manually recorded signals, separate attention/freshness/lifecycle states, no calculated overall percentage, recoverable detachment, and a one-way conflict-aware mirror. These are proposed implementation contracts pending engineering review, not additional claims of already shipped behavior.

Next gate: plan-eng-review, including a design/accessibility pass before UI implementation. After review acceptance, split the module into tracker tickets. Do not create tickets or start implementation as part of writing this spec.
