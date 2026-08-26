# Design: Private Review Briefs and Conditional Author Feedback

Date: 2026-08-26
Repo: cockpit
Mode: Builder

## Problem Statement

Cockpit's `summary` field currently serves two incompatible audiences:

1. The reviewer needs a fast private briefing that explains the change, its
   complexity, its boundary crossings, its blast radius, and the remaining
   uncertainty.
2. The PR author may need a concise top-level GitHub message, but only when
   there is an actionable concern that is broader than a line-level finding.

The shipped review skill currently optimizes `summary` for an encouraging,
author-facing reaction. In practice this produces long praise-heavy prose that
is neither an efficient reviewer briefing nor consistently useful to post.

Cockpit is currently a personal tool. The primary product goal is therefore to
improve the reviewer's understanding and decision quality. Team workflows and
commercial use are explicitly deferred.

## What Makes This Cool

A completed review should read like a compact engineering risk briefing. In
roughly 15 seconds, the reviewer should understand:

- what changed and how it works;
- which areas are most complex, vulnerable, or uncertain;
- which system, package, service, persistence, or ownership boundaries changed;
- how failures propagate and what the blast radius is;
- what the tests prove and what remains unproven; and
- whether to approve, investigate, or request changes—and why.

Public feedback becomes quieter and more intentional. Cockpit should not post
generic praise or narrate the author's own change back to them. It should
produce a top-level author message only when a genuine cross-cutting concern
cannot be expressed adequately as a localized inline comment.

## Premises

1. The private review brief is always generated and is never posted to GitHub.
2. The brief is structured so each risk dimension is explicitly considered.
3. Inline findings remain the home for localized, actionable feedback.
4. Positive feedback does not qualify as high-level feedback.
5. A public author message exists only for an actionable concern spanning the
   overall design, multiple files, or a system boundary.
6. With no high-level concern, Cockpit posts no top-level prose—only the chosen
   verdict and selected inline comments.
7. The machine-readable `verdict` remains authoritative; the brief explains
   the reasoning rather than inventing a second verdict system.

## Approaches Considered

### A. Dual prose fields

Add a private Markdown `review_brief` and an optional public
`author_message`.

Effort: Medium

Advantages:

- Fastest implementation.
- Establishes the private/public separation.
- Leaves the model flexibility for unusual PRs.

Disadvantages:

- Brief structure and quality can drift between reviews.
- Risk dimensions may be omitted silently.
- Long prose remains slower to scan.

### B. Structured risk brief

Generate a structured private brief covering the change, hotspots,
boundaries, blast radius, validation, uncertainty, and recommendation. Keep a
separate optional `author_message`.

Effort: Large

Advantages:

- Predictable and fast to scan.
- Forces explicit consideration of every important risk dimension.
- Supports purpose-built dashboard presentation later.
- Makes missing analysis visible rather than hiding it in prose.

Disadvantages:

- Requires changes to the skill contract, persistence, queries, and UI.
- Small PRs will legitimately have empty sections.

### C. First-class high-level findings

Model high-level concerns as selectable review items alongside inline
findings, then compose the public message from the selected concerns.

Effort: Large

Advantages:

- Gives the reviewer maximum control over public feedback.
- Allows severity and selection state for architectural concerns.
- Avoids trusting an opaque generated paragraph.

Disadvantages:

- Adds significant review-management UI.
- Composed prose may feel mechanical.
- Is more machinery than a personal tool currently needs.

## Recommended Approach

Use Approach B: a structured private risk brief plus an optional author-facing
message.

### Proposed output contract

The default skill should emit the existing PR metadata, verdict, findings,
positives, and follow-ups, plus:

```json
{
  "review_brief": {
    "change": {
      "intent": "Why the change exists.",
      "mechanism": "How the implementation achieves it."
    },
    "risk_level": "low | medium | high",
    "risk_rationale": "Why this level fits.",
    "complex_areas": [
      {
        "area": "Component or code path",
        "why": "What makes it complex, vulnerable, or hard to verify"
      }
    ],
    "boundary_changes": [
      {
        "boundary": "Caller -> dependency, service -> service, or code -> storage",
        "impact": "New coupling, contract, failure mode, or ownership concern"
      }
    ],
    "blast_radius": "What can fail, who is affected, and how far failure propagates.",
    "validation": {
      "coverage": "What the supplied tests or checks demonstrate.",
      "gaps": ["Important behavior that remains unproven."]
    },
    "uncertainties": ["Assumptions the review could not verify."],
    "recommendation": "The next decision or investigation, with rationale.",
    "high_level_concerns": [
      {
        "concern": "Cross-cutting actionable concern.",
        "why": "Why an inline comment is insufficient."
      }
    ]
  },
  "author_message": null
}
```

`author_message` is `null` or empty when `high_level_concerns` is empty. When
concerns exist, it is a concise author-ready message addressing only those
concerns.

### High-level concern definition

A concern qualifies when it affects the change as a whole or spans multiple
locations, such as:

- a new synchronous dependency on a request or ingestion hot path;
- a trust, authorization, or data-ownership boundary that changed;
- failure handling whose consequences span multiple components;
- a rollout, migration, or compatibility risk not anchored to one line; or
- a test-strategy gap that undermines confidence in the overall change.

The following do not qualify:

- generic praise;
- a recap of what the PR does;
- verdict language;
- a restatement of one inline finding; or
- an optional improvement with no material risk.

### Persistence and compatibility

- Store the structured brief as JSON in a new review column so its shape can
  evolve without creating a column per section.
- Store the optional author message separately from the private brief.
- Keep reading the legacy `summary` field for existing and custom-skill
  reviews during a compatibility window.
- Never reinterpret a private brief as a public message.
- Existing pending summaries remain explicitly labeled as legacy content so
  they cannot silently cross the new private/public boundary.

### Dashboard and detail behavior

- Replace the current praise-oriented summary preview with a private risk
  brief headed by risk level and change intent.
- Make complex areas, boundary changes, blast radius, validation gaps, and
  uncertainties independently scannable.
- Keep inline findings grouped by severity as they are today.
- Show the author message editor only when a high-level concern exists or the
  reviewer explicitly chooses to add a message.
- Label the distinction plainly: `Private brief — never posted` and
  `Message to author — posted as review body`.

### Posting invariants

- The private brief must never enter a GitHub payload.
- A non-empty author message must correspond to at least one high-level
  concern.
- With no author message, approval posts only the verdict and selected inline
  comments.
- Request-changes and comment-only events must continue to obey GitHub's body
  and comment requirements; exact empty-body behavior should be verified
  before implementation.
- The reviewer can edit or remove an author message before posting.

## Open Questions

1. Should risk level be model-assigned, rule-derived from findings and
   boundaries, or model-assigned with deterministic guardrails?
2. Should a high-level concern carry severity and selection state in the first
   version, or is presence/absence sufficient for a personal tool?
3. How should custom skills declare whether they support the new contract?
4. What is the safest compatibility treatment for already-pending legacy
   summaries?
5. Does GitHub accept an empty top-level body for every verdict Cockpit
   supports, especially request changes?

## Success Criteria

- A reviewer can identify the change, principal risks, boundary changes, blast
  radius, and recommendation within 15 seconds.
- Every generated review explicitly addresses all structured brief sections,
  even when the answer is “none identified.”
- Generic praise never causes an author message to be generated.
- `author_message` is empty whenever no high-level concern exists.
- The GitHub posting path cannot read or submit the private brief.
- Inline findings remain independently selectable and postable.
- Existing reviews and custom skills fail safely rather than publishing a
  private brief as public prose.

## Next Steps

1. Verify GitHub's empty-body rules for approve, comment, and request-changes
   events.
2. Resolve the five open questions, prioritizing compatibility and posting
   safety.
3. Turn this design into an implementation spec covering the skill contract,
   migration, persistence, dashboard, detail page, submission path, and tests.
