#!/usr/bin/env bash
# Resolves the tag a release run publishes and writes it to $GITHUB_OUTPUT as tag=<tag>.
#
# A tag push releases the pushed tag. A dispatched run releases v$VERSION: it creates the tag in the
# local checkout only, and GoReleaser creates it on GitHub together with the release, so a failed run
# leaves no tag behind. The tag's version without pre-release suffix must match the version in
# flake.nix, so the Nix package does not lag behind the releases.
set -euo pipefail

semver='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'

if [[ "${GITHUB_REF:?GITHUB_REF is required}" == refs/tags/* ]]; then
  tag="${GITHUB_REF#refs/tags/}"
else
  version="${VERSION:?VERSION is required for a dispatched release}"
  tag="v${version#v}"
fi

if [[ ! "$tag" =~ $semver ]]; then
  echo "::error::$tag is not a semantic version tag like v0.1.0"
  exit 1
fi

flake_version="$(sed -n 's/^ *version = "\(.*\)";$/\1/p' flake.nix)"
core="${tag#v}"
core="${core%%-*}"
if [[ "$flake_version" != "$core" ]]; then
  echo "::error::flake.nix has version ${flake_version:-none}; set it to $core before releasing $tag"
  exit 1
fi

if [[ "$GITHUB_REF" != refs/tags/* ]]; then
  if git rev-parse --quiet --verify "refs/tags/$tag" >/dev/null; then
    echo "::error::$tag already exists"
    exit 1
  fi
  git tag "$tag"
fi

echo "tag=$tag" >>"${GITHUB_OUTPUT:?GITHUB_OUTPUT is required}"
