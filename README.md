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
nix build 'path:.'                # ./result/bin/dev-inbox
nix run 'path:.' -- serve         # run without installing
go install ./cmd/dev-inbox        # to $(go env GOBIN)
```

The binary is static (no cgo; SQLite is pure Go).

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
| `--preferred-team` | none | `org/slug`; PRs authored by its members score +30 |
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
  preferred-team: acme/platform
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
- `greylist: []` and `--greylist=` both mean no greylist. Likewise `--preferred-team=` turns off a team set in the
  file.

#### Score weights

The points each priority term adds to a PR's score are settable only in the file, under `serve.score`, and have no
flags. Keys that are absent keep their default, so list only the ones to change:

```yaml
serve:
  score:
    urgent: 20              # a PR-level linear.urgency annotation exists
    preferred-team: 30      # the author is a member of the preferred team
    greylist: -15           # the author is greylisted
    direct: 10              # review is requested from you personally
    checks-running: 5       # any check is still running
    checks-success: 20      # the checks rollup is successful
    per-failing-check: -10  # per failing check
    merge-conflict: -40     # the PR has merge conflicts
```

The values shown are the defaults. A value must be an integer: `0` disables a term and a negative value ranks PRs
down. An unknown key or a non-integer value is an error. `dev-inbox config create` includes the block.

## TUI keys

| Key | Action |
|---|---|
| `j` / `k`, arrows | Move down / up |
| `g` / `G` | Jump to top / bottom |
| `enter` / `o` | Open the PR in the browser (expands a collapsed section header) |
| `y` | Copy the PR URL |
| `tab` / `shift+tab` | Next / previous tab (wraps around) |
| `1` / `2` | Jump to the "My PRs" / "Others' PRs" tab |
| `d` | Toggle the detail pane |
| `J` / `K`, `pgdn` / `pgup` | Scroll the detail pane |
| `space`, or `h` / `l` | Fold section, or collapse / expand |
| `r` | Refresh |
| `?` | Help |
| `esc` | Close help, otherwise the detail pane |
| `q` | Quit |

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

## Development

```sh
just build   # bin/dev-inbox
just test
just lint
just fmt
nix develop  # shell with go, gopls, golangci-lint, just, natscli
```

Tests that talk to the real GitHub API are skipped unless `DEVINBOX_LIVE=1` is set. After changing Go
dependencies, set `vendorHash` in `flake.nix` to `nixpkgs.lib.fakeHash`, run
`nix build 'path:.#default'` and paste the reported hash. Keeping the old hash silently reuses the
cached dependencies and the build fails with "inconsistent vendoring".
