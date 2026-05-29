# Contributing to zdev

Thanks for picking this up. zdev is a personal-tooling project that grew
into something other people might want, so the bar is "useful and
predictable" rather than "feature-complete". Drive-by patches welcome.

## Dev loop

```sh
git clone git@github.com:tristankenney/zdev.git
cd zdev/zdevd
make test     # plutil lint + anti-fork gates + go test -race ./...
make build    # builds the three binaries into zdevd/bin/
make install  # symlinks into ~/.local/bin and bootstraps launchd
```

`make test` is what CI runs; if it's green locally it'll be green in CI.

## Style

- **Go**: `gofmt`-clean; `go vet ./...` passes; `staticcheck` is welcome
  but not gated. Standard library where reasonable — the dependency tree
  is intentionally small.
- **Shell**: bash for everything except `bin/zdev` and `bin/zdev-pick` (zsh
  for associative arrays). `set -euo pipefail` at the top of every script.
- **Commits**: present-tense imperative subject line under ~72 chars; a
  body explaining *why* if the change isn't obvious. Don't co-author Claude
  / Copilot / etc. into commits.
- **Tests for behaviour, not implementation.** New events / state shapes
  should have unit tests at the `applyEvent` level (see
  `internal/hub/state_test.go` for the pattern). Don't mock unless you
  must.

## Things that block a PR

- `make test` fails
- A test was deleted without explaining why
- A new dependency is added without a one-line justification in the PR
- A change to a launchd plist, the socket protocol, or the on-disk
  `zdevd-state.json` schema lacks a migration story

## Things that don't block a PR

- Doc typos
- "Could be cleaner" — say so as a comment, don't gate the merge

## Release flow

Maintainer-only. Tag `vX.Y.Z` on a clean main; CI must be green; push the
tag and create a GitHub Release with notes that include any breaking
changes (especially socket-protocol bumps).

## Getting unstuck

File an issue with what you tried, what you expected, and what you got.
For daemon issues, include `~/Library/Logs/zdev/zdevd.err.log` and
`launchctl print gui/$(id -u)/com.zdev.zdevd` output.
