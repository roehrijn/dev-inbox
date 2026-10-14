# dev-inbox design

Terminal mimic of the GitHub PR inbox (github.com/pulls), backed by an event-driven daemon.

## Overview

```
GitHub (notifications, search, GraphQL)
        │  gh CLI credentials, read-only
        ▼
dev-inbox serve (daemon)
  github ─► ingest worker ─► derive.Diff ─► store.ApplySnapshot (PR + comments + outbox, one tx)
                                                    │
                                     bus relay ◄────┘  outbox → JetStream (CloudEvents, dedup by ID)
                                        │
        embedded NATS (127.0.0.1:14222) ├─► stream DEVINBOX_EVENTS: devinbox.events.>
                                        ├─► request/reply API: devinbox.api.*
                                        └─► in-process Go handlers (durable consumers)
        ▲
        │ NATS (same API as any external tool)
dev-inbox (TUI)
```

- One binary: `dev-inbox serve` runs the daemon, `dev-inbox` runs the TUI.
- The TUI is a pure NATS client. External tools use the same subjects.
- GitHub state is never mutated; read/done state of GitHub notifications stays untouched.
- github.com only, single account.

## Packages

| Package | Responsibility |
|---|---|
| `internal/model` | Shared domain types and event constants. The contract; change only deliberately. |
| `internal/api` | NATS subjects, request/response types, CloudEvents envelope, event subject scheme. |
| `internal/store` | SQLite (modernc.org/sqlite, pure Go): PR snapshots, comments, annotations, outbox, meta. |
| `internal/github` | gh-authenticated client: notifications, search, GraphQL snapshots and statuses. |
| `internal/derive` | Pure functions: section classification and snapshot diff to domain events. |
| `internal/bus` | Embedded NATS + JetStream, outbox relay, API serving, in-process handlers. |
| `internal/daemon` | Wiring: loops, ingest worker, API handlers. |
| `internal/tui` | Bubble Tea v2 / Bubbles v2 / Lip Gloss v2 / Glamour v2 inbox. |
| `cmd/dev-inbox` | CLI entry point. |

Exported signatures in the package stubs are the contract between packages. Adding exports is fine;
changing existing ones needs a deliberate cross-package change.

## Tracking

A PR becomes tracked when a notification arrives whose subject is a pull request and whose reason is
not `subscribed` (watched repo), or when the initial backfill finds it. Notifications with reason
`subscribed` refresh PRs that are already tracked. A PR stays tracked until it is merged or closed;
afterwards it remains in the store but is no longer refreshed or listed, unless a notification
reveals it was reopened.

Ingestion has three sources:

1. Notifications poll (`GET /notifications?all=true&since=...`, `If-Modified-Since`), interval
   `max(--poll-interval, X-Poll-Interval)`, default 60s. Discovery trigger.
2. Backfill once on an empty store: search `involves:@me`, `review-requested:@me`, `reviewed-by:@me`
   (open PRs, unarchived repos). Backfilled PRs only emit `pr.discovered` so workflows are not
   flooded with history.
3. Reconcile every 5 minutes: `FetchStatuses` for all tracked open PRs; a PR whose status differs
   from the stored `PRStatus` gets a full fetch. Catches silent changes (CI, mergeability, merge queue).
   Open PRs that currently need the viewer are fully refetched every cycle as well (at most 30, most
   recently updated first), since reactions change no `updatedAt` and send no notification.

All three feed one ingest worker (serialized, deduplicated queue): `FetchPR` → stored snapshot →
`derive.Diff` → `store.ApplySnapshot` → `bus.Kick`.

## Sections (derive.Classify)

Open PRs only; closed and merged get `SectionNone`. Logins compare case-insensitively. "Mine" means
`pr.Author == viewer.Login`.

| Section | Rule (first match wins) |
|---|---|
| `review_requested` | Not mine and a pending review request names the viewer (`Direct=true`) or one of the viewer's teams. |
| `involved` | Not mine and no request for the viewer or their teams. |
| `drafts` | Mine and draft. |
| `needs_action` | Mine and any reason: checks `FAILURE` ("checks failing"), review decision `CHANGES_REQUESTED` ("changes requested"), mergeable `CONFLICTING` ("merge conflict"), new comments ("new comments"). |
| `ready_to_merge` | Mine and in merge queue, or merge state `CLEAN`/`HAS_HOOKS` and (decision `APPROVED`, or decision empty with no pending review requests and at least one `APPROVED` review). |
| `waiting_for_review` | Mine, otherwise. |

New comments: comments by someone other than the viewer, not a bot (login suffix `[bot]`) and not a
greylisted login (bot accounts that are plain users, such as `loft-bot`), created
after the viewer's last activity = latest of `LastPushAt`, the viewer's own comments and reviews.

## Priority (derive.Score)

Every classified PR carries an integer `Score`; inside a section the TUI orders by score descending,
then `UpdatedAt` descending. The daemon computes it, so the list API and every event payload carry it.

`Evaluate` also returns the non-zero contributions as `Classification.ScoreTerms` (label and points, in
evaluation order, summing to `Score`), so the list API, events and the TUI detail pane can explain a score.

Points per term are configurable in the config file only (`serve.score.<key>`); 0 disables a term.
The TUI hides PRs of others with a negative score and shows how many it hid per section; the API and
events still carry them.

| Term | Key | Default |
|---|---|---|
| PR-level annotation with key `linear.urgency` exists (any value) | `urgent` | +20 |
| Author is a member of a preferred team (incl. child teams); the highest points of the author's teams, not the sum | `serve.preferred-teams` (`org/slug: points`), not under `score` | none |
| Author is greylisted (`serve --greylist login`, repeatable, default `dependabot`, `renovate`; matched case-insensitively with or without `[bot]`) | `greylist` | -15 |
| Review requested from the viewer personally (`Direct`, ★) | `direct` | +10 |
| At least one check queued, in progress, pending, waiting or expected (`ChecksRunning > 0`) | `checks-running` | +5 |
| Check rollup is `SUCCESS` (skipped and neutral count as success; no bonus without checks) | `checks-success` | +20 |
| Per check concluded FAILURE, ERROR, TIMED_OUT, CANCELLED, STARTUP_FAILURE or ACTION_REQUIRED | `per-failing-check` | -10 each |
| GitHub reports the PR as `CONFLICTING` (merge conflicts; `UNKNOWN` adds nothing) | `merge-conflict` | -40 |
| The PR is a draft | `draft` | -5 |

Check counts come from the rollup's per-state counts, so they are not capped like the listed check
runs, and they are part of `PRStatus`: reconciliation refetches a PR when they change. GitHub's
rollup also counts superseded check runs: every job of a workflow run that a newer run of the same
workflow and event replaced, such as one cancelled by its successor, and check runs a newer one of
the same name replaced. Those are found among the first 100 listed check runs and subtracted, and the
check state is then derived from the corrected counts. Comparing whole runs also drops jobs that a
run cancelled early never got to name, such as unexpanded matrix jobs, which `gh pr checks` still
reports as failing.

Members of the preferred teams are fetched at startup and every 6 hours, and cached in the store for
offline starts. A team that is not found or not accessible contributes nothing; other fetch failures keep
the team's previous members and retry after a minute. Score changes caused by membership changes emit no
event.

## Hiding (derive.HiddenKey)

The viewer hides a PR until something new happens in it with the PR-level annotation `devinbox.hidden`,
set through `devinbox.api.annotate` like any other annotation (TUI key `x`; a `null` value unhides). Its
value is a `derive.HiddenMark` of the snapshot the viewer saw, built by `derive.HiddenAnnotation`:

```json
{"seen": "<FetchedAt>", "head": "<HeadSHA>", "draft": false, "requested": true}
```

`Evaluate` sets `Classification.Hidden` while nothing is new compared to that mark:

- the head is unchanged (new commits, a rebase or a force-push change it),
- the PR was not marked ready for review (`draft` true, now false),
- review was not newly requested from the viewer personally (`requested` false, now `Direct`),
- no comment or review by someone other than the viewer, a bot or a greylisted login is newer than `seen`,
- no PR-level annotation with a non-empty `attention` list was updated after the hide annotation itself
  (its `UpdatedAt`, not `seen`). This holds for every plugin; plugins write only on change, so an unchanged
  annotation does not undo a hide.

Checks, mergeability, labels, reactions and the viewer's own activity are not new. Using the snapshot's
`FetchedAt` rather than the time of hiding means activity the daemon had not fetched yet when the viewer hid
the PR still counts as new. Once something new happened the annotation stays but no longer hides; hiding
again replaces it. Hidden PRs keep their section, score and reasons; the TUI lists them in a collapsed
"Hidden until new activity" section per tab, and the flag is part of the list API and every event payload.

## Events (derive.Diff)

All events are produced by `model.NewEvent` with `model.EventData`: the snapshot after the change
(without comments), its classification including the score, and an event-specific `Change`.

| Type | When | Change |
|---|---|---|
| `pr.discovered` | `old == nil` | none |
| `pr.review_requested` | Request for the viewer or one of their teams appears (also on discovery) | `ChangeReviewRequest` |
| `pr.review_request_removed` | Such a request disappears | `ChangeReviewRequest` |
| `pr.commits_pushed` | `HeadSHA` changed | `ChangeCommits` |
| `pr.review_submitted` | Review ID not seen before, one event per review, oldest first | `Review` |
| `pr.comment_added` | Comment ID not seen before, one event per comment, oldest first | `Comment` |
| `pr.checks_failed` | `Checks` becomes `FAILURE` | `ChangeChecks` |
| `pr.checks_passed` | `Checks` becomes `SUCCESS` | `ChangeChecks` |
| `pr.draft_changed` | `IsDraft` flipped | `ChangeDraft` |
| `pr.merged` / `pr.closed` / `pr.reopened` | State transition | none |
| `pr.section_changed` | `Classify(old) != Classify(cur)` section | `ChangeSection` |
| `pr.updated` | Last, whenever anything changed (snapshot compared without `Comments` and `FetchedAt`) | none |
| `pr.annotated` | Annotation set or deleted via the API (daemon, not derive) | `Annotation` |

On discovery only `pr.discovered` and `pr.review_requested` are emitted, never history such as
existing comments. During backfill the daemon keeps only `pr.discovered`.

## Event stream

- Transport: embedded NATS with JetStream, listening on `127.0.0.1:14222`, no auth. Stream
  `DEVINBOX_EVENTS` captures `devinbox.events.>`, file storage, 30 day max age, 2 minute duplicate
  window keyed by `Nats-Msg-Id` = event ID.
- Envelope: CloudEvents 1.0 structured mode (`api.CloudEvent`), header
  `Content-Type: application/cloudevents+json`.
  - `type`: `devinbox.<event>`, e.g. `devinbox.pr.merged`
  - `source`: `https://github.com/<owner>/<repo>`; `subject`: `pull/<number>`
  - `data`: `model.EventData`
- Subjects: `devinbox.events.pr.<owner>.<repo>.<number>.<event>`; owner and repo lowercased, `.`
  escaped as `%2E`. Examples: `devinbox.events.pr.*.*.*.merged`, `devinbox.events.pr.acme-co.>`.
- Delivery: store and outbox are written in one SQLite transaction; the relay publishes pending
  events in order and marks them published after the JetStream ack. At-least-once; consumers dedup
  by CloudEvent `id` if they care.
- Quick look: `nats --server nats://127.0.0.1:14222 sub 'devinbox.events.>'`.

## API (request/reply, JSON)

| Subject | Request | Response |
|---|---|---|
| `devinbox.api.inbox.list` | `ListRequest` | `ListResponse`: viewer + all open tracked PRs, no comments, PR-level annotations |
| `devinbox.api.pr.get` | `GetRequest` | `GetResponse`: one PR with comments and all annotations |
| `devinbox.api.annotate` | `AnnotateRequest` | `AnnotateResponse`; JSON null value deletes |
| `devinbox.api.pr.track` | `TrackRequest` | `TrackResponse`: unknown PR is queued for ingestion and replied to at once (`pr.discovered` follows), known PR is a no-op with `known: true` |

Failures reply with `{"error": "..."}`.

## Extension

- External workflows: any NATS client; durable JetStream consumers resume after downtime.
- In-process Go handlers: `bus.Handle(ctx, name, filterSubject, fn)`, registered in the daemon.
- Enrichment: annotations via `devinbox.api.annotate` (or the store from in-process handlers through
  the daemon). Namespaced keys such as `linear.urgency`. A JSON object value with a `badge` string and
  optional `level` (`info`, `warn`, `crit`) is rendered as a badge by the TUI; the detail pane lists
  all annotations. A JSON object value with a `links` array of `{id, title, url, detail}` objects (and an
  optional `label`, default `Links`) is shown as a section in the TUI details, and its links open with
  `O` (a picker when several), whether they point to Linear issues, Slack messages or anything else. Example: `linear.issues` is
  `{"label": "Linear issues", "links": [{"id": "ACME-1", "title": "Fix login", "url": "https://linear.app/acme/issue/ACME-1/fix-login", "detail": "In Progress"}]}`.
  Neither convention scores by itself; a PR-level `score` or `attention` field in the value does.

Plugins are separate binaries that talk to the daemon only through these subjects: `dev-inbox-jev`
(comment intent), `dev-inbox-linear` (due dates and linked issues) and `dev-inbox-slack` (review-channel tags;
requests unknown PRs with `pr.track`, annotates `slack.mentions`). See
[proposals/slack.md](proposals/slack.md).

Workflow ideas: [ideas.md](ideas.md).

## State

`$XDG_STATE_HOME/dev-inbox` (default `~/.local/state/dev-inbox`): `inbox.db` (SQLite, WAL) and
`jetstream/`. Published events older than 30 days are pruned from the outbox daily.
