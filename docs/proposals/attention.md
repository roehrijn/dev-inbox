# Proposal: "needs me" attention for foreign PRs

Status: draft, revised after design review and user decisions.

## Goal

The "Others' PRs" tab shows only PRs that need something from the viewer: a review, a re-review, or
an answer. A separate plugin daemon uses TypeSafe Jev to read comment intents and refines what
"needs me" means and how urgent it is. The core daemon and TUI stay fully usable when no plugin is
running, and fall back to deterministic signals.

## Overview

```
dev-inbox serve (core)                                   dev-inbox-jev (plugin, optional)
  derive: deterministic attention signals ─┐               durable JetStream consumer "jev"
  derive: annotation contributions        ├─ reasons       on comment/review/push events
  events ─────────────────────────────────►┼──────────────► classify new comments via Jev
  devinbox.api.annotate ◄──────────────────┼─────────────── comment annotations  jev.intent
                                           │                PR annotation       jev.attention
TUI Others' tab: PRs with attention reasons; `a` toggles "all"; status lists live plugins
```

Two layers:

1. **Core (always on):** derives attention signals from snapshot data alone. No AI, no new
   dependency.
2. **Plugin (optional):** classifies the comments those signals point at, adds attention the core
   cannot see (questions without a mention), and suppresses core noise (a "thanks" reply). It uses
   only the public NATS API, like any external workflow.

Principle: **attention reasons decide visibility, the score only orders.** The score mixes unrelated
terms (merge conflict -40, checks, preferred team), so it must not gate whether a question reaches me.
A PR with at least one reason *needs me*: it gets a flat `needs-me` bonus (+20) on top of its signal
weights and is never hidden, in any view, even when its total score is negative.

## Core changes

### Last activity and attention signals (derive, foreign open PRs)

- *My last activity*: latest of my submitted reviews, my comments, and my inline comments.
- *My head*: the commit of my latest review or inline comment, whichever is later
  (`PullRequestReview.commit.oid`, `PullRequestReviewComment.originalCommit.oid`; an inline
  comment's `commit` follows the PR head while its thread is not outdated, so it cannot be used).
  Both are nullable; a missing commit means "unknown" and disables the SHA-based signal for that PR.

Signals become reasons in `Classification.Reasons` (today only used for needs_action; foreign
sections never have those, so one field covers both and the TUI already renders it):

| Signal | Rule | Reason | Weight key (default) |
|---|---|---|---|
| review requested | pending request names me directly | `review requested` | existing `direct` (+10) |
| team review requested | no direct request; a pending request names one of my teams; human, non-greylisted author; not draft; updated within 14 days; no review yet by a human other than the author (bot and greylisted reviews ignored); checks not failing; no merge conflict | `team review requested` | `team-review` (+5) |
| re-review | my head is known and differs from `HeadSHA`, and since my last activity: my latest review is CHANGES_REQUESTED or DISMISSED, or someone else replied in a thread I started or replied in, or such a thread was resolved by someone else | `changes since your review` | `rereview` (+30) |
| thread reply | someone else (not a bot) replied in a thread I started or replied in after my last activity, and no re-review signal | `reply to your comment` | `thread-reply` (+15) |
| mentioned | comment by someone else after my last activity containing `@<me>` on word boundaries | `mentioned you` | `mentioned` (+20) |

A comment is *handled* by me when I left an acknowledging reaction on it (👍, ❤️, 🎉, 🚀; not 👀,
😕, 👎, 😄), when I replied later in the same review thread (inline comments) or posted a later
top-level comment (other comments), or when I submitted a later formal review (approval, change
request, dismissal, or a review with a summary body; GitHub's bare thread-reply reviews do not count).
The wording of my reply does not matter: once I answered, the ball is in the other court. Handled
comments feed neither the thread-reply and mention signals nor the plugin's intents. Reactions bump
no `updatedAt` and send no notification, so reconcile fully refetches PRs that currently need me
(at most 30 per cycle), and a changed reaction emits `pr.updated`.

Comments carrying a comment-level annotation whose value contains `"attention": false` are ignored
by the thread-reply and mention signals. That is how a plugin suppresses noise without knowing the
core's rules and without negative points.

### Annotation contributions (derive)

A PR-level annotation whose JSON value contains

- `"score": <int>` adds it to the score (clamped to ±100),
- `"attention": ["<reason>", ...]` adds reasons (at most 5, 60 characters each),

but only while it is fresh: `annotation.UpdatedAt` is later than my last activity. Acting on a PR
therefore retires every plugin contribution on the next ingest, even when the plugin is down.
`linear.urgency` keeps its fixed weight and is not subject to this rule.

### Data model and fetching

- `model.Review.CommitSHA`, `model.Comment.CommitSHA` (inline comments), and per review thread
  `IsResolved` and `ResolvedBy` (GraphQL `reviewThreads { isResolved resolvedBy { login } }`).
- Review threads are paginated instead of `last: 50`, since my oldest threads are the ones that
  matter for re-review. `comments(last: 100)` logs when capped.
- No migration: snapshots are JSON; stored reviews gain the fields on their next full fetch, and a
  push (the case that matters) always triggers one.

### TUI

- Others' tab default view "needs me": foreign PRs with at least one reason, regardless of score.
  Involved PRs without a reason are hidden there. `a` toggles to "all", which applies the existing
  negative-score hiding only to PRs without reasons; PRs that need me are never hidden.
- Reasons render where they render today (row line 2, accent). Plugin badges need no change:
  `prBadges` already renders any PR-level annotation with a `badge`.
- Status bar lists live plugins found by pinging all NATS micro services (`$SRV.PING`) on the
  existing minute tick, e.g. `devinbox-jev ●`. The core knows "plugins", not Jev.

### Config

- New `serve.score.*` weights: `needs-me` (+20), `rereview` (+30), `thread-reply` (+15),
  `mentioned` (+20).
- A `plugins:` namespace the core ignores: skipped by the unknown-key check, and decoded into a
  `Plugins map[string]any` field so `UnmarshalExact` accepts it.

## Plugin: dev-inbox-jev

Separate binary in this repo (`cmd/dev-inbox-jev`), separate process, same Nix package. Depends only
on `internal/api` contracts and NATS; registers as NATS micro service `devinbox-jev`. Reads
`TYPESAFE_API_KEY` from the environment only, never from the config file.

### Flow

1. Start: `inbox.list` → viewer and PRs. Sweep foreign open PRs.
2. Durable consumer `jev` with `FilterSubjects` for `comment_added`, `review_submitted`,
   `commits_pushed`, `review_requested`, `discovered`, `reopened` (never `annotated`: no feedback
   loop). Debounce per PR (2s).
3. Per PR: `pr.get` → candidates = comments by someone else, not a bot, after my last activity, in a
   thread I started or replied in, mentioning me, or top-level on a PR with a review request or
   review from me. Comments that already carry `jev.intent` are not reclassified.
4. One batched Jev call per PR (one Choice per candidate); write `jev.intent` per comment, adding
   `"attention": false` for `ack` and `not_for_me`.
5. Write `jev.attention` with score and reasons, or delete it (`null`) when nothing is needed.

### Intents

State per question: PR title, PR author, viewer login, the comment, its thread including my original
finding, whether it mentions me.

| Option | Meaning | PR contribution | Comment annotation |
|---|---|---|---|
| `question_for_me` | asks me something or wants my input | +40, `question for you` | intent |
| `pushback` | disagrees with or questions my finding | +30, `pushback on your comment` | intent |
| `rereview_request` | asks me to look again | +30, `asks for re-review`; 0 if the core already has `changes since your review` (no double count; +30 then only for "look again" without a push) | intent |
| `addressed` | reports my finding as fixed | 0 (core re-review owns the +30) | intent |
| `ack` | thanks or acknowledgement only | 0 | intent, `"attention": false` |
| `not_for_me` | informational or addressed to someone else | 0 | intent, `"attention": false` |

Below probability 0.6 a result is `unclear`: no contribution, no suppression. Per PR, each intent
counts once (max per intent, summed across distinct intents), so a chatty PR cannot dominate.
Weights, threshold and options live in `plugins.jev.*`.

### User's example

I requested changes; the author replied "fixed, please have a look" in my thread and pushed. Core:
`changes since your review`, +30. Plugin: `rereview_request`, 0 because the core reason is present.
Total attention contribution +30, with or without the plugin.

## Degradation

| Situation | Behavior |
|---|---|
| No plugin installed | Core signals drive the view; questions without a mention in top-level comments are missed. |
| Plugin down | Its contributions stay until I act, then expire on the next ingest; the durable consumer catches up on restart (30 day stream). |
| Jev API down or rate limited | Plugin naks with backoff; core unaffected. |
| I acted on a PR | My own comment or review sends no notification, so the daemon learns of it at the next reconcile (at most 5 minutes); core signals and plugin contributions clear then. |

## Cost and load

One Jev call per PR with new candidates, 70-500 ms, a few thousand tokens at $0.042 per million input
tokens: well below a cent per day. Each annotate call emits `pr.annotated` and a debounced TUI
re-list; fine at this volume, no batch API needed.

## Data protection

Comment bodies and PR titles from private repositories leave the machine to TypeSafe AI; make sure that
is acceptable for your organization. An optional org/repo allowlist (`plugins.jev.repos`, default: all)
limits what is sent.

## Phases and tests

1. Core: data model and fetching (commit SHAs, thread resolution, thread pagination), signals,
   annotation contributions with freshness and clamps, `needs-me` bonus, TUI needs-me view and
   toggle, plugin status, `plugins:` namespace. Tests: derive table tests per signal including
   last-activity anchoring, null commits, suppression and freshness; github fixtures for the new
   fields and pagination; TUI tests for the view, toggle and status; config tests for the namespace.
2. Plugin: candidate selection, Jev HTTP client (verify the wire format; no Go SDK), PR result
   computation, micro service registration. Tests against a fake Jev server and an embedded NATS.
3. Tuning: label ~50 real comments, measure accuracy (third-party benchmarks report ~0.78 on intent
   classification), adjust options and threshold.

## Decisions

1. PRs that need me get +20 (`needs-me`) and are never hidden, in any view, regardless of score.
2. Direct review requests always count. A team request counts only while the PR waits for a reviewer
   and is ready (see the `team review requested` signal); otherwise it surfaces only through the
   other signals. Analysis of 91 team-only requests: 17 bot-authored, 2 drafts, ~47 untouched for 14+
   days, 12 already picked up by a human, 3 failing CI, 10 waiting and ready.
3. Involved PRs without a reason are hidden in the needs-me view and visible under "all".
