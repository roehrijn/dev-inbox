# Agent notes

See [CONTRIBUTING.md](CONTRIBUTING.md) for building, testing, commit conventions and releases.

## Releases

Before releasing a version, set `version` in [flake.nix](flake.nix) to it, without a pre-release
suffix (`v0.3.0` and `v0.3.0-rc.1` both need `0.3.0`), and merge that change to `main` first.
[release-tag.sh](.github/scripts/release-tag.sh) rejects a release whose version differs from the
flake, so the Nix package and the `--version` of its binaries do not lag behind the GitHub releases.
