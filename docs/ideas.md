# Workflow ideas

Open ideas, not implemented. Each fits the event stream and annotation API described in
[design.md](design.md).

## Comment intent via TypeSafe Jev

Classify PR comments (`pr.comment_added`) and comments of linked Linear issues with Jev typed
questions, e.g. a Choice of blocking / question for me / nit / approval / FYI, and annotate the
comment with `jev.intent`. Could feed a "needs my answer" badge.

## AI review trigger

On `pr.review_requested` with `direct: true` (or `pr.discovered` in section `review_requested`), start
a Claude Code `pr-review` run for the PR. External subscriber with a durable consumer.

## Slack messages

Post or DM on selected events, e.g. `pr.checks_failed` on own PRs, `pr.section_changed` to
`ready_to_merge`.

## Auto-actions on own PRs

Re-request review after `pr.commits_pushed` addressing changes requested; enable auto-merge on
`pr.section_changed` to `ready_to_merge`. These would be the first workflows that write to GitHub.
