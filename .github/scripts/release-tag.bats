#!/usr/bin/env bats

setup() {
  script="$BATS_TEST_DIRNAME/release-tag.sh"
  export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1
  cd "$BATS_TEST_TMPDIR"
  git init --quiet repo
  cd repo
  git -c user.name=t -c user.email=t@t commit --quiet --allow-empty -m init
  flake_version 0.1.0
  export GITHUB_OUTPUT="$BATS_TEST_TMPDIR/output"
  : >"$GITHUB_OUTPUT"
}

flake_version() {
  printf '{\n  packages.default = {\n    version = "%s";\n  };\n}\n' "$1" >flake.nix
}

@test "a dispatched version is tagged locally" {
  GITHUB_REF=refs/heads/main VERSION=0.1.0 run "$script"
  [ "$status" -eq 0 ]
  [ "$(cat "$GITHUB_OUTPUT")" = "tag=v0.1.0" ]
  [ "$(git rev-parse v0.1.0)" = "$(git rev-parse HEAD)" ]
}

@test "a dispatched version may start with v" {
  flake_version 0.2.0
  GITHUB_REF=refs/heads/main VERSION=v0.2.0-rc.1 run "$script"
  [ "$status" -eq 0 ]
  [ "$(cat "$GITHUB_OUTPUT")" = "tag=v0.2.0-rc.1" ]
}

@test "a dispatched version that is not semver is rejected" {
  for v in 0.1 01.0.0 0.1.0+build latest ""; do
    GITHUB_REF=refs/heads/main VERSION="$v" run "$script"
    [ "$status" -ne 0 ]
  done
  [ -z "$(git tag)" ]
  [ ! -s "$GITHUB_OUTPUT" ]
}

@test "a dispatched version that is already tagged is rejected" {
  git tag v0.1.0
  GITHUB_REF=refs/heads/main VERSION=0.1.0 run "$script"
  [ "$status" -ne 0 ]
  [[ "$output" == *"v0.1.0 already exists"* ]]
}

@test "a pushed tag is released as is" {
  flake_version 1.2.3
  git tag v1.2.3
  GITHUB_REF=refs/tags/v1.2.3 VERSION= run "$script"
  [ "$status" -eq 0 ]
  [ "$(cat "$GITHUB_OUTPUT")" = "tag=v1.2.3" ]
}

@test "a pushed tag that is not semver is rejected" {
  GITHUB_REF=refs/tags/v1.2 run "$script"
  [ "$status" -ne 0 ]
  [[ "$output" == *"not a semantic version"* ]]
}

@test "a version that differs from flake.nix is rejected" {
  GITHUB_REF=refs/heads/main VERSION=0.2.0 run "$script"
  [ "$status" -ne 0 ]
  [[ "$output" == *"flake.nix has version 0.1.0; set it to 0.2.0"* ]]
  [ -z "$(git tag)" ]
  [ ! -s "$GITHUB_OUTPUT" ]
}

@test "a pushed tag that differs from flake.nix is rejected" {
  git tag v0.2.0
  GITHUB_REF=refs/tags/v0.2.0 run "$script"
  [ "$status" -ne 0 ]
  [ ! -s "$GITHUB_OUTPUT" ]
}
