# dev-inbox

A terminal mimic of the GitHub PR inbox (github.com/pulls). A daemon polls GitHub through your `gh`
credentials, tracks the PRs that involve you, and publishes every change as a CloudEvent on an embedded
NATS server with JetStream. The TUI is a plain NATS client of that daemon; any other tool can subscribe
to the same events or call the same API. GitHub state is never modified.

## Requirements

- The [`gh` CLI](https://cli.github.com), logged in to github.com (`gh auth login`). Only github.com and a
  single account are supported.
- Go 1.26 or newer, or Nix with flakes, to build.

## Install

```sh
nix build                                       # ./result/bin/dev-inbox and ./result/bin/dev-inbox-jev
nix run . -- serve                              # run without installing
go install ./cmd/dev-inbox ./cmd/dev-inbox-jev  # to $(go env GOBIN)
just build                                      # ./bin/dev-inbox and ./bin/dev-inbox-jev
```

`nix build` sees only files tracked by git; use `nix build 'path:.'` to include uncommitted changes. The
binaries are static (no cgo; SQLite is pure Go). `dev-inbox-jev` is the optional [Jev plugin](#jev-plugin).

## Quick start

1. Log in to GitHub with the `gh` CLI. dev-inbox reuses its token and only reads from GitHub:

   ```sh
   gh auth login
   ```

2. Write a config file and uncomment what you need, at least `serve:` and your teams (see
   [Config file](#config-file)):

   ```sh
   mkdir -p ~/.config/dev-inbox
   dev-inbox config create > ~/.config/dev-inbox/config.yaml
   ```

   ```yaml
   serve:
     preferred-teams:
       acme/my-team: 30
       acme/engineering: 10
   ```

3. Start the daemon and keep it running:

   ```sh
   dev-inbox serve
   ```

   On the first start it imports every open PR that involves you (search for `involves:`, `review-requested:` and
   `reviewed-by:` you), which takes a few minutes for a few hundred PRs. Progress is logged; the store remembers
   when the import is done, and later starts skip it.

4. Open the TUI in another terminal. It fills up while the import runs and updates live afterwards:

   ```sh
   dev-inbox
   ```

5. Optionally start the Jev plugin next to the daemon:

   ```sh
   TYPESAFE_API_KEY=... dev-inbox-jev
   ```

The daemon, the TUI and the plugin find each other on `nats://127.0.0.1:14222`; start them in any order. The TUI and
the plugin reconnect when the daemon restarts.

### Running as a service

The daemon and the plugin are long-running and log to stderr, so a systemd user service fits them. For example
`~/.config/systemd/user/dev-inbox.service`:

```ini
[Unit]
Description=dev-inbox daemon
After=network-online.target

[Service]
ExecStart=%h/go/bin/dev-inbox serve
# Needed when gh keeps its token in the system keyring, see below.
Environment=GH_PATH=/usr/bin/gh
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

and `~/.config/systemd/user/dev-inbox-jev.service`, with the API key in a file only you can read
(`~/.config/dev-inbox/jev.env` containing `TYPESAFE_API_KEY=...`, mode 0600):

```ini
[Unit]
Description=dev-inbox Jev plugin
After=dev-inbox.service
Wants=dev-inbox.service

[Service]
ExecStart=%h/go/bin/dev-inbox-jev
EnvironmentFile=%h/.config/dev-inbox/jev.env
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now dev-inbox dev-inbox-jev
journalctl --user -u dev-inbox -f
```

The daemon takes the token from `GH_TOKEN` or `GITHUB_TOKEN`, then from `~/.config/gh/hosts.yml`, and otherwise
runs `gh auth token`. That last case, the default when `gh` stores the token in the system keyring, needs `gh` on
the service's `PATH` or its path in `GH_PATH`; a user service usually has neither, so set `GH_PATH` (find it with
`command -v gh`). Adjust `ExecStart` to where the binaries are, e.g. a Nix profile. The plugin logs
private PR content (see [Jev plugin](#jev-plugin)), and so does the journal then.

### Upgrading

Rebuild, then restart the daemon, the plugin and every open TUI; running processes keep the old binary.
Stored PRs gain new fields on their next full fetch from GitHub. If they look incomplete or wrong after an upgrade,
stop the daemon and delete the state directory (default `~/.local/state/dev-inbox`); the next start imports
everything again. That loses annotations, such as hidden PRs and plugin results, and the event history.

## Usage

Start the daemon, then the TUI in another terminal:

```sh
dev-inbox serve
dev-inbox
```

| Command | Description |
|---|---|
| `dev-inbox` | Run the inbox TUI |
| `dev-inbox serve` | Run the daemon |
| `dev-inbox config create` | Print a config file template to stdout |

`dev-inbox serve` flags:

| Flag | Default | Description |
|---|---|---|
| `--state-dir` | `$XDG_STATE_HOME/dev-inbox` | Directory of the database and the JetStream storage |
| `--nats-host` | `127.0.0.1` | Listen address of the embedded NATS server |
| `--nats-port` | `14222` | Listen port of the embedded NATS server |
| `--poll-interval` | `1m` | Minimum interval between GitHub notification polls |
| `--reconcile-interval` | `5m` | Interval between status reconciliations of open PRs |
| `--log-level` | `info` | `debug`, `info`, `warn` or `error` |
| `--preferred-teams` | none | `org/slug=points`; PRs authored by the team's members get the points; comma-separated or repeatable, replaces the file's teams |
| `--greylist` | `dependabot`, `renovate` | Login whose PRs score -15; repeatable, replaces the defaults; empty (`--greylist=`) disables |

Inside each section PRs are ordered by priority score, then last activity; see
[docs/design.md](docs/design.md#priority-derivescore) for the terms.

`dev-inbox` (TUI) flag:

| Flag | Default | Description |
|---|---|---|
| `--nats-url` | `nats://127.0.0.1:14222` | NATS URL of the daemon |

Both commands also take `--config <path>`, see below.

State lives in `$XDG_STATE_HOME/dev-inbox` (default `~/.local/state/dev-inbox`): `inbox.db` (SQLite) and
`jetstream/`. The embedded NATS server has no authentication; keep it on loopback.

### Config file

Every flag above can also be set in a YAML file, with one section per command and keys named like the flags:

```yaml
serve:
  poll-interval: 2m
  preferred-teams:
    acme/platform: 30
    acme/eng: 10
  greylist: [dependabot, renovate, some-bot]
tui:
  nats-url: nats://127.0.0.1:14222
```

The file is read from `$XDG_CONFIG_HOME/dev-inbox/config.yaml`, or `~/.config/dev-inbox/config.yaml` when
`XDG_CONFIG_HOME` is unset. Write a template with every setting commented out at its default:

```sh
dev-inbox config create > ~/.config/dev-inbox/config.yaml
```

Precedence is a flag given on the command line, then the config file, then the built-in default. There are no
environment variables.

- A missing default file is ignored. A missing file given with `--config`, invalid YAML, and unknown keys are
  errors.
- A leading `~/` in `state-dir` expands to the home directory.
- `greylist: []` and `--greylist=` both mean no greylist.
- `preferred-teams` maps GitHub teams (`org/slug`, members of child teams included) to the points PRs of their
  members get; negative points rank them down. An author in several teams gets the highest points, not the sum.
  Members are fetched at startup and every 6 hours. Teams given with `--preferred-teams` replace those of the file.

#### Score weights

The points each priority term adds to a PR's score are settable only in the file, under `serve.score`, and have no
flags. Keys that are absent keep their default, so list only the ones to change:

```yaml
serve:
  score:
    urgent: 20              # a PR-level linear.urgency annotation exists
    greylist: -15           # the author is greylisted
    direct: 10              # review is requested from you personally
    checks-running: 5       # any check is still running
    checks-success: 20      # the checks rollup is successful
    per-failing-check: -10  # per failing check
    merge-conflict: -40     # the PR has merge conflicts
    draft: -5               # the PR is a draft
    needs-me: 20            # another author's PR has at least one attention reason
    rereview: 30            # another author's PR changed since your review
    thread-reply: 15        # someone replied in a review thread you started or joined
    mentioned: 20           # someone mentioned you after your last activity
    team-review: 5          # a review is requested from one of your teams and the PR waits for a reviewer
```

The values shown are the defaults. A value must be an integer: `0` disables a term and a negative value ranks PRs
down. An unknown key or a non-integer value is an error. `dev-inbox config create` includes the block.

The last five terms rank PRs of other authors that need something from you. Each attention reason (`review
requested`, `team review requested`, `changes since your review`, `reply to your comment`, `mentioned you`) adds
its own weight, and a PR with at least one reason also gets `needs-me`. Reasons decide which PRs show up, the score
only orders them, so a PR that needs you is not hidden by a negative score. See
[docs/proposals/attention.md](docs/proposals/attention.md).

A team review request counts as a reason (`team-review`) only while the PR waits for a first review and is ready for
one, and no direct request to you applies: it is not a draft, was updated within the last 14 days, has no failing
checks and no merge conflict, and its author is neither a bot nor greylisted. Any review by someone who is not a bot,
not greylisted and not the author ends the wait. Pending checks are fine.

#### Plugins

The top-level `plugins` key is reserved for external plugins. The core ignores it: nothing below `plugins` is
checked, so a plugin can define whatever keys it needs, for example `plugins.jev`. Unknown keys outside `plugins`
are still errors.

```yaml
plugins:
  jev:
    threshold: 0.6
```

## TUI keys

| Key | Action |
|---|---|
| `j` / `k`, arrows | Move down / up |
| `g` / `G` | Jump to top / bottom |
| `enter` / `o` | Open the PR in the browser (expands a collapsed section header) |
| `y` | Copy the PR URL |
| `x` | Hide the PR until something new happens in it, or unhide it |
| `tab` / `shift+tab` | Next / previous tab (wraps around) |
| `1` / `2` | Jump to the "My PRs" / "Others' PRs" tab |
| `a` | On the "Others' PRs" tab, toggle between "needs me" (default) and "all" |
| `d` | Toggle the detail pane |
| `J` / `K`, `pgdn` / `pgup` | Scroll the detail pane |
| `space`, or `h` / `l` | Fold section, or collapse / expand |
| `r` | Refresh |
| `?` | Help |
| `esc` | Close help, otherwise the detail pane |
| `q` | Quit |

The "Others' PRs" tab starts in the "needs me" view, which lists only PRs with an attention reason (see
[Config file](#config-file)). The "all" view adds the other PRs you are involved in, except those with a negative
score. A PR that needs you is listed in both views, and the section headers count the PRs left out ("N hidden").

`x` moves a PR into the collapsed "Hidden until new activity" section at the end of its tab, in either view. It
comes back by itself on new commits (including a rebase or force-push), a comment or review by someone else (bots
and greylisted logins excluded) that you have not seen, when it is marked ready for review, or when your review is
newly requested. Checks, merge conflicts, labels and your own activity do not bring it back. The hidden state is
a local annotation; GitHub is not touched. The section is only shown while it holds PRs.

The detail pane (`d`) lists how the score adds up, one line per term, e.g. `team acme-co/web-platform-core +30`.

Row symbols before the title: `★` review requested from you personally, `✓` / `✗` / `●` checks passed / failing /
running, `◌` draft.

The status bar lists every NATS micro service that answers a `$SRV.PING`, such as a plugin, e.g. `devinbox-jev ●`.

## Event stream

Every change to a tracked PR is published to the JetStream stream `DEVINBOX_EVENTS` (file storage, 30 day
retention). Subjects look like

```
devinbox.events.pr.<owner>.<repo>.<number>.<event>
```

for example `devinbox.events.pr.acme.api.42.merged`. Owner and repo are lowercased, and `.` inside them is
escaped as `%2E`. Messages are CloudEvents 1.0 in structured mode (`application/cloudevents+json`):

```json
{
  "specversion": "1.0",
  "id": "...",
  "source": "https://github.com/acme/api",
  "type": "devinbox.pr.merged",
  "subject": "pull/42",
  "time": "2026-10-11T12:00:00Z",
  "datacontenttype": "application/json",
  "data": { }
}
```

`data` holds the PR snapshot after the change, its inbox section, and an event-specific change. Delivery
is at-least-once; dedup on the CloudEvent `id` if that matters to you. Event types and their payloads are
listed in [docs/design.md](docs/design.md#events-derivediff).

Watch everything with the [`nats` CLI](https://github.com/nats-io/natscli):

```sh
nats --server nats://127.0.0.1:14222 sub 'devinbox.events.>'
```

Filter with subject wildcards:

```sh
nats --server nats://127.0.0.1:14222 sub 'devinbox.events.pr.acme.>'        # one org
nats --server nats://127.0.0.1:14222 sub 'devinbox.events.pr.*.*.*.merged'  # merges only
```

For workflows that must not miss events while offline, use a durable JetStream consumer on the stream.

### API

Request/reply with JSON bodies. Failures reply with `{"error": "..."}`.

| Subject | Request | Response |
|---|---|---|
| `devinbox.api.inbox.list` | `{}` | Viewer and all open tracked PRs with their section and PR-level annotations |
| `devinbox.api.pr.get` | `{"pr": {"owner": "...", "repo": "...", "number": 1}}` | One PR with comments and all annotations |
| `devinbox.api.annotate` | `{"annotation": {"pr": {...}, "key": "ns.key", "value": ...}}` | Empty on success; a `null` value deletes the annotation |

```sh
nats --server nats://127.0.0.1:14222 request devinbox.api.inbox.list '{}'
```

Annotation values shaped like `{"badge": "due in 2d", "level": "warn"}` render as a badge in the TUI. See
[docs/design.md](docs/design.md) for the architecture and [docs/ideas.md](docs/ideas.md) for workflow ideas.

## Jev plugin

`dev-inbox-jev` is an optional plugin that reads what new comments on others' PRs want from you, using
[TypeSafe Jev](https://docs.typesafe.ai). It runs next to `dev-inbox serve` and uses only the NATS API above:

```sh
go install ./cmd/dev-inbox-jev
TYPESAFE_API_KEY=... dev-inbox-jev
```

The API key is read from `TYPESAFE_API_KEY` only, never from the config file. The plugin looks at comments by
someone else, not a bot, that you have not handled and that are in a review thread you started or joined, mention
you, or are top-level on a PR you are directly requested to review or have reviewed. You handle a comment by
reacting to it with 👍, ❤️, 🎉 or 🚀, by replying later in the same review thread (a top-level comment: by a later
top-level comment), or by submitting a later formal review. It classifies each such comment once as
`question_for_me`, `pushback`, `rereview_request`, `addressed`, `ack` or `not_for_me`, stored as the comment
annotation `jev.intent`. `ack` and `not_for_me` also mute the comment for the core's `reply to your comment` and
`mentioned you` reasons. The PR annotation `jev.attention` then adds a score and the reasons `question for you`,
`pushback on your comment` and `asks for re-review`, or is deleted when nothing is needed. A re-review request adds
nothing while the core already reports `changes since your review`. Handling a comment removes its intent from the
result, while its `jev.intent` annotation stays. Everything the plugin added expires as soon as you act on the PR
and is written again right away if comments you have not handled remain.

Flags: `--config` (same default file as dev-inbox), `--nats-url`, `--log-level`. Settings live under
`plugins.jev`; unknown keys there are errors:

```yaml
plugins:
  jev:
    threshold: 0.6           # below this probability a comment is "unclear" and adds nothing
    weights:                 # points per intent, each counted once per PR; 0 disables an intent
      question-for-me: 40
      pushback: 30
      rereview-request: 30
      addressed: 0
      ack: 0
      not-for-me: 0
    repos: [acme, other/web] # orgs or org/repos sent to Jev; absent or empty sends all
    debounce: 2s             # quiet time after an event before a PR is evaluated
    # nats-url and log-level work like the flags; nats-url falls back to tui.nats-url
```

Comment bodies and PR titles, including those of private repositories, are sent to TypeSafe AI; `repos` limits
what leaves the machine. Every Jev request and response, bodies included and the API key never, is logged as a
JSON line on stderr for later analysis. The plugin registers the NATS micro service `devinbox-jev`, which the TUI
status bar shows while it runs. Its durable JetStream consumer `jev` catches up on missed events after a restart,
and events whose evaluation fails, for example while Jev is rate limited, are redelivered with backoff. A PR is
evaluated again on every event that can change which comments are unhandled, including `pr.updated`, which the core
emits when your reactions change, but never on `pr.annotated`, so the plugin does not trigger itself.

## Development

```sh
just build   # bin/dev-inbox
just test
just lint
just fmt
nix develop  # shell with go, gopls, golangci-lint, just, natscli
```

Tests that talk to the real GitHub API are skipped unless `DEVINBOX_LIVE=1` is set. After changing Go
dependencies, or importing a package not imported before (even from a module already in `go.mod`),
set `vendorHash` in `flake.nix` to `nixpkgs.lib.fakeHash`, run `nix build 'path:.#default'` and paste
the reported hash. Keeping the old hash silently reuses the cached vendor directory and the build
fails with "inconsistent vendoring" or "cannot find module providing package".
