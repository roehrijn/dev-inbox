# Proposal: Slack review requests

Status: accepted, in implementation.

## Goal

When someone links a PR in a review channel (e.g. `#dev-pr-reviews`) and tags the viewer, either
directly or through a Slack user group the viewer belongs to, the PR enters the inbox if it is not
there yet, and it gains 10 points while it waits for the viewer. Slack stays out of the core: a plugin
daemon, `dev-inbox-slack`, reads Slack and talks to the daemon only through the public NATS API,
like `dev-inbox-jev` and `dev-inbox-linear`.

## Overview

```
Slack Web API                      dev-inbox-slack (plugin)                 dev-inbox serve (core)
  conversations.history  ◄──poll── collect tagging messages per PR
  conversations.replies            PR unknown? ──────────── devinbox.api.pr.track ──► queue, FetchPR,
  users.info                                                                         pr.discovered
                                   PR known    ──────────── devinbox.api.annotate ──► slack.mentions
```

## Core change: `devinbox.api.pr.track`

The core can only annotate PRs it already knows, and it only learns of PRs through GitHub
(backfill, notifications). A new, Slack-agnostic request lets any client ask for a PR:

```go
SubjectTrack = "devinbox.api.pr.track"

type TrackRequest struct{ PR model.PRKey `json:"pr"` }
type TrackResponse struct {
	Known bool   `json:"known,omitempty"` // already in the store, nothing queued
	Error string `json:"error,omitempty"`
}
```

- Unknown PR: the handler pushes `item{key}` (not baseline) onto the ingestion queue and returns at
  once. Ingestion emits `pr.discovered` as usual, so the Linear plugin picks the PR up too.
- Known PR: no-op, `known: true`.
- Inaccessible PRs end as today: `settle` logs and drops `github.ErrNotFound`.
- After that the PR is an ordinary tracked PR. Viewers without involvement get no GitHub
  notifications for it, but reconcile refreshes every open stored PR every 5 minutes, and watched
  repos refresh tracked PRs through `subscribed` notifications.
- A foreign PR the viewer is not involved in classifies as `SectionInvolved` already, so nothing in
  derive changes.
- `pluginkit.Client.Track(ctx, key) (known bool, err error)`.

The request is useful beyond Slack, e.g. for a later TUI "add PR by URL".

## Plugin: dev-inbox-slack

Separate binary `cmd/dev-inbox-slack`, packages `internal/slack` (Web API client) and
`internal/slackplugin`, same Nix package and Justfile build. NATS micro service `devinbox-slack`,
annotation Source `dev-inbox-slack`.

### Slack access

No new Slack app: the plugin signs in through the OAuth user flow of an existing app, the one whose
client ID other MCP clients of the user already use for Slack's MCP server. That flow is Slack's
standard user OAuth (`slack.com/oauth/v2_user/authorize`, `oauth.v2.user.access`), and its tokens
work against the Web API directly, so the plugin needs no MCP. Verified with such a token:
`auth.test`, `conversations.history` (200 messages per page, no 15-message cap) and
`conversations.replies` work; `usergroups.list` fails with `missing_scope`, the app does not offer
`usergroups:read`.

- `dev-inbox-slack login` runs the flow like the MCP clients do: PKCE (S256), no client secret,
  `resource=https://mcp.slack.com`, redirect `http://localhost:3118/callback` (registered for the
  app, so the port is fixed), user scopes `channels:history groups:history channels:read groups:read
  users:read`.
- Tokens are short-lived (`xoxe.`, about 12 hours) with single-use rotating refresh tokens. The
  plugin keeps its own in `$XDG_STATE_HOME/dev-inbox/slack-token.json` (mode 0600, written
  atomically), refreshes shortly before expiry or on `token_expired`, and never logs them. It does
  not share the token of another client, since a refresh would invalidate the other client's copy.
- The client ID is private. It is read from `oauth-client-id` in `slack.yaml` or `SLACK_CLIENT_ID`
  (which wins), has no default in code, and tests use fake IDs.
- Without `usergroups:read` the plugin cannot list the viewer's groups, so `groups` in `slack.yaml`
  lists the IDs (`S...`) of the groups that count.

Startup: `auth.test` gives the viewer's Slack user ID and the workspace URL (for permalinks).

### Polling, not Socket Mode

Every `poll-interval` (1m) the plugin reads each channel's messages of the last `lookback` (7 days)
with `conversations.history`, all pages. Replies come from `conversations.replies` for thread
parents whose `latest_reply` changed since the plugin last read the thread; the plugin caches the
replies in memory. The busiest review channel has more than 200 messages per week and about half of
them have threads, so the first poll reads many threads; the client waits out `Retry-After` on HTTP
429, so it only takes longer. Later polls read one or two history pages and the few changed threads.

Polling over Socket Mode because the laptop is offline at night: re-reading a window catches up
without a persistent cursor, edits and deletions are picked up for free, and there is one code path.
Latency matches the GitHub notifications poll.

### Tagging messages

A message tags the viewer if its text contains `<@VIEWER_ID>` or `<!subteam^GROUP_ID>` of a group in
`groups`, either with a `|label` suffix. Not counted: `@here`, `@channel`, messages by
the viewer, bot messages.

PRs are taken from links `https://github.com/<owner>/<repo>/pull/<n>` in the text (Slack writes
`<url>` or `<url|label>`). A tagging thread reply without a PR link inherits the links of its thread
parent ("@alice can you take this one?").

### Per PR

Each poll recomputes the full state from the window, so the plugin persists nothing but its token:

1. Group tagging messages by PR.
2. `pr.get`. Unknown PR: `pr.track`, remembered in memory for an hour so an inaccessible PR is not
   requested every minute; the annotation follows on a later poll, once the PR is known.
3. Keep the tagging messages newer than the viewer's last activity on the PR
   (`derive.LastActivity`, as the Jev plugin does), so a review settles earlier tags.
4. Write the `slack.mentions` annotation when it changed, delete it (`null`) when none are left. A
   new tag after a review changes the links, so the rewritten annotation is fresh again.

```json
{
  "score": 10,
  "attention": ["tagged you in #dev-pr-reviews"],
  "label": "Slack",
  "links": [{"id": "@alice", "title": "can someone from @reviewers take this?",
             "url": "https://acme-co.slack.com/archives/C0123/p1760000000000100",
             "detail": "#dev-pr-reviews · Oct 12 19:02"}]
}
```

- `score`: `score` (10) per PR, however many messages tag the viewer.
- `attention`: `tagged you in #<channel>` for a direct tag, `tagged your team in #<channel>` for a
  group, deduplicated. This makes the PR *need me*, so it shows in the default Others view and
  gets the flat `needs-me` +20: a tagged PR gains +30 in total.
- `links`: permalinks of the counted messages, so the details pane lists them and `O` opens them
  (the picker title becomes "Open link").

Freshness and expiry come from the core: the annotation contributes only while newer than the
viewer's last activity, and drops out of the window after `lookback`.

### Config: `$XDG_CONFIG_HOME/dev-inbox/slack.yaml`

```yaml
nats-url: nats://127.0.0.1:14222
log-level: info
oauth-client-id: "..."       # private, never in this repo; SLACK_CLIENT_ID wins
channels: [dev-pr-reviews]   # names or IDs
groups: [S0123ABCD]          # IDs of user groups whose tags count
score: 10
poll-interval: 1m
lookback: 168h
```

## Core change: attention unhides

`derive.hidden` only looks at GitHub activity, so a PR hidden with `x` would stay hidden on a new
tag. New rule: a PR-level annotation with a non-empty `attention` list that was updated after the
hide annotation (its `UpdatedAt`, the time of hiding; not `HiddenMark.Seen`, the fetch time, which
an annotation written between fetch and hide would beat) unhides the PR. This applies to every
plugin, so a new Jev question unhides too. Plugins write only on change, so an unchanged annotation
does not undo a hide.

## Degradation

| Situation | Behavior |
|---|---|
| Plugin not running | No Slack signal; annotations already written stay until the viewer acts. |
| Slack down or rate limited | Poll fails, logged, retried on the next tick (honoring `Retry-After`). |
| Daemon down | Poll skips annotate and track, the next poll catches up. |
| PR not accessible with the GitHub token | Daemon logs and drops it; plugin retries hourly. |
| Token expired and refresh failed | Poll fails with a hint to run `dev-inbox-slack login`. |

## Data protection

Only requests to Slack leave the machine. Message excerpts (link titles) and the author's display
name land in the local SQLite store, like PR content already does.

## Phases and tests

1. Core: `pr.track` handler, API types, `pluginkit.Client.Track`, attention unhides. Tests: daemon
   test that an unknown key is ingested and emits `pr.discovered`, a known key is a no-op; derive
   table tests for unhiding (attention newer or older than the hide, annotation without attention).
2. Plugin: Slack client against an `httptest` fake (login, refresh and rotation, token store,
   history, replies, pagination, `Retry-After`),
   mention and link parsing table tests (user, group with and without label, `@here`, own
   messages, thread inheritance), per-PR computation, embedded NATS harness like the Linear plugin.
3. TUI: picker title "Open link"; README and design docs.

## Decisions

1. Tagging adds an attention reason, so the PR needs me (+10 and the `needs-me` +20).
2. Score per PR, not per message.
3. A newer annotation with attention unhides a hidden PR.
4. Tags on the viewer's own PRs add nothing, since annotation scores count only on foreign PRs.
