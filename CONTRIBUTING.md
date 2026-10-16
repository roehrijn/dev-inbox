# Contributing

## Development

```sh
just build             # bin/dev-inbox, bin/dev-inbox-jev, bin/dev-inbox-linear and bin/dev-inbox-slack
just test
just lint
just fmt
just release-snapshot  # the release build without publishing, into dist/
nix develop            # shell with go, gopls, golangci-lint, just, natscli
```

The [README](README.md#development) covers live GitHub tests and updating `vendorHash` in `flake.nix`.

Commit subjects follow [Conventional Commits](https://www.conventionalcommits.org) in lowercase, e.g.
`fix: replace dead http/2 connections after standby`. The release notes are built from them: `feat` goes
under Features, `fix` under Fixes, and `docs`, `test`, `chore` and `ci` are left out.

Every push to `main` and every pull request runs [CI](.github/workflows/ci.yaml): build (also for macOS),
tests, golangci-lint, the tests of the release scripts and a check of the GoReleaser config.

## Releases

Versions follow [semantic versioning](https://semver.org) and tags are `v<version>`, e.g. `v0.1.0`. Go
needs all three numbers, so release 0.1 is `v0.1.0`. While the major version is 0, a minor bump may
break config, state or the event API; say so in the release notes. A version with a suffix such as
`v0.2.0-rc.1` is published as a pre-release.

### Cutting a release

Release from the head of `main` after its CI passed, in one of two ways:

- **From GitHub**: Actions → Release → Run workflow on `main`, enter the version (`0.1.0`), or from a
  terminal:

  ```sh
  gh workflow run release.yaml --ref main -f version=0.1.0
  gh run watch "$(gh run list --workflow release.yaml --limit 1 --json databaseId --jq '.[0].databaseId')"
  ```

- **By pushing a tag**, which can also release a commit other than the head of `main`:

  ```sh
  git tag -a v0.1.0 -m v0.1.0
  git push origin v0.1.0
  ```

The [release workflow](.github/workflows/release.yaml) then:

1. runs the full CI on the commit;
2. resolves the tag with [release-tag.sh](.github/scripts/release-tag.sh). For a run started from GitHub it
   checks that the version is valid and not released yet, and tags the commit in its own checkout only;
3. runs [GoReleaser](.goreleaser.yaml), which builds the four binaries for Linux and macOS on amd64 and
   arm64, packs one `dev-inbox_<version>_<os>_<arch>.tar.gz` per platform with `checksums.txt`, and
   publishes the GitHub release with notes grouped from the commit subjects. GitHub creates the tag with
   the release, so a run that fails earlier leaves no tag behind;
4. attests the build provenance of every archive.

Edit the generated notes on the release page afterwards if a change needs explaining, such as a breaking
change or an upgrade step.

### Verifying a release

```sh
gh release download v0.1.0 --repo roehrijn/dev-inbox --pattern '*linux_amd64.tar.gz' --pattern checksums.txt
sha256sum --check --ignore-missing checksums.txt
gh attestation verify dev-inbox_0.1.0_linux_amd64.tar.gz --repo roehrijn/dev-inbox
tar xzf dev-inbox_0.1.0_linux_amd64.tar.gz && ./dev-inbox --version
```

### When a release fails

- **Before GoReleaser publishes** (CI, the version check or the build fails): nothing exists on GitHub yet.
  Fix the cause on `main` and run the release again. A tag you pushed yourself must be deleted first
  (`git push origin :refs/tags/v0.1.0`) or the fix released as the next patch version.
- **After the release is published** (for instance the attestation step fails): rerun the failed job if
  the failure was transient. Otherwise delete the release and its tag
  (`gh release delete v0.1.0 --cleanup-tag --yes`) and release again, or leave it and release the next
  patch version. Never move a published tag; `go install` and checksum databases cache what it pointed to.

### Repository settings

Already in place through the workflow files, nothing to configure: the release job asks for the
`contents`, `id-token` and `attestations` write permissions it needs, and every other job only reads.
Recommended once: a tag ruleset for `v*` that blocks updates and deletions. Do not restrict tag creation
there unless GitHub Actions may bypass it, or GoReleaser cannot create the tag of a dispatched release.
