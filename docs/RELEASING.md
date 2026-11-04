# Releasing cp3

One tag releases the binary: GitHub release, Homebrew tap, deb/rpm/apk/archlinux.

1. `go test ./...`
2. `git tag vX.Y.Z && git push origin main vX.Y.Z`
3. `GITHUB_TOKEN=$(gh auth token) mise x go@1.27.0 -- goreleaser release --clean` (the shim is pinned to that Go; goreleaser does not read gh's token on its own).
4. Fleet: `ssh <host> 'sh -s' < install.sh` per machine, `cp3 doctor` on each.
