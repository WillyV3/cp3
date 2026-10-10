#!/bin/sh
# Refuses a release whose mod version does not match the tag. The mod's
# plugin.json version is what Claude Code pins users to, so a tag that
# ships without bumping it leaves every installed mod on the old code.
# goreleaser runs this as a before hook: check-mod-version.sh <version> <isSnapshot>
set -eu
want=$1
snapshot=${2:-false}
have=$(sed -n 's/.*"version": *"\([^"]*\)".*/\1/p' "$(dirname "$0")/../mod/.claude-plugin/plugin.json")
if [ "$snapshot" = "true" ]; then
  echo "mod version $have (snapshot build, tag not checked)"
  exit 0
fi
if [ "$have" != "$want" ]; then
  echo "mod/.claude-plugin/plugin.json says $have but the release is $want — bump it and commit before tagging" >&2
  exit 1
fi
echo "mod version $have matches release $want"
