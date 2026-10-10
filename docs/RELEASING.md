# Releasing cp3

One tag releases everything: the binary (GitHub release, Homebrew tap, deb/rpm/apk/archlinux)
and the Claude Code mod, which users install from this repository as a plugin marketplace.

1. Bump `mod/.claude-plugin/plugin.json` `version` to the release version and commit.
   Claude Code pins installed mods to that version, so a release that forgets this leaves
   every installed mod on the old code. goreleaser refuses the release when it does not match
   (`scripts/check-mod-version.sh`, a before hook).
2. `claude plugin validate . --strict && claude plugin validate mod --strict && claude plugin test mod`
3. `go test ./...`
4. `git tag vX.Y.Z && git push origin main vX.Y.Z`
5. `mise x go@1.27.0 -- goreleaser release --clean` (the goreleaser shim is pinned to that Go).
6. Fleet: `ssh <host> 'sh -s' < install.sh` per machine, `cp3 doctor` on each.
   Mod: `claude plugin update cp3@cp3` on each machine that has it, or `cp3 setup --mod` on one that does not.

A snapshot build (`goreleaser release --snapshot --clean`) skips the version check.
