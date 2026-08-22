# Adversarial codebase review — 2026-08-17

Status refreshed: 2026-08-18 (Australia/Melbourne), against `main` at
`291c4ae` plus the current uncommitted worktree.

## Scope

This review examined the current branch and worktree adversarially, with
particular attention to destructive commands, path and repository identity,
Git reachability, configuration-controlled command execution, and the hub's
state-management invariants.

No code changes were made as part of the review.

## Merge-gating findings

### 1. `stream rm` permits path traversal and deletion outside the workspace

Location: `bin/zdev:925`

`stream_path` only needs to contain `/`. Values containing `..` are joined
directly to `$WORKSPACE` and the result is eventually passed to `rm -rf`.
The command does not canonicalise the paths, reject symlink escapes, or prove
that the deletion target remains inside the selected initiative.

Causal chain:

1. The user supplies a path such as `../some-directory`.
2. The path passes the current contains-a-slash check.
3. Shell path joining resolves the target outside the intended initiative or
   workspace.
4. If the loose identity check also passes, `rm -rf` deletes that target.

Required fix:

- Accept exactly two validated path segments: `<initiative>/<stream>`.
- Reject absolute paths, empty or dot segments, `..`, extra depth, whitespace,
  control characters, and option-like names.
- Resolve workspace, initiative, and target paths canonically, including
  symlinks.
- Prove the target is strictly beneath the selected initiative and is neither
  the initiative nor workspace root before any teardown or deletion.
- Print and revalidate the exact resolved target immediately before removal.

### 2. An ordinary repository can be mistaken for a stream and deleted

Location: `bin/zdev:926`

The identity check rejects a target only when both its `.pay/stack.yml` marker
and the directory itself are absent. Consequently, any existing directory is
accepted even if it is an ordinary initiative member rather than a stream.

The dirty and unpushed loop examines repositories one directory below the
target. When the target itself is a repository, the loop sees no repository to
check. The command then removes the target recursively at `bin/zdev:961`.

Causal chain:

1. A user passes an ordinary member such as `<initiative>/<repository>`.
2. Directory existence satisfies the current identity condition.
3. The safety loop does not inspect the target repository itself.
4. Dirty files, untracked files, and local commits are not detected.
5. `rm -rf` irreversibly deletes the working copy.

Required fix:

- Require a zdev-owned stream marker with a validated schema and expected
  initiative/stream identity.
- Never treat directory existence as proof of stream identity.
- Reject ordinary repositories, initiative roots, workspace roots, malformed
  markers, and symlinked targets without mutation.
- Put the destructive operation behind a narrowly testable helper or an
  exhaustive temporary-workspace shell contract suite.

### 3. Branches without an upstream are treated as safely pushed

Location: `bin/zdev:938`

The removal guard calculates the ahead count using:

```sh
git rev-list --count @{upstream}..HEAD 2>/dev/null || echo 0
```

When a branch has no upstream, `git rev-list` fails and the fallback converts
"unknown" into zero unpushed commits. This is especially likely for the
primary branch created by `stream add` at `bin/zdev:889`, because `checkout -b`
does not configure an upstream.

Causal chain:

1. `stream add` creates a new primary branch without an upstream.
2. The user commits locally but does not push with upstream tracking.
3. The upstream query fails.
4. The failure is converted to an ahead count of zero.
5. The stream and its only copy of the commits are deleted.

Required fix:

- Fail closed when no upstream is configured.
- Block detached HEAD unless its commit is proven reachable from an allowed
  remote reference.
- Treat failed Git inspection as unsafe, not clean.
- Check dirty, untracked, ahead, merge/rebase-in-progress, submodule-dirty,
  and otherwise unreachable commits.
- Provide a separate, explicit confirmation flow if intentionally disposable
  branches need an override.

## Worth-fixing findings

### 4. A partial built-in agent override erases every omitted field

Location: `zdevd/internal/config/config.go:216`

The configuration documentation says that agent entries overlay built-ins and
that every field except `name` is optional. The implementation nevertheless
uses `out[i] = spec`, replacing the entire built-in entry.

For example, an entry intended only to customise Claude's glyph silently
removes Claude's waiting, finished, and spinner markers, along with its probe
command and launch command. Detection and automatic launch then stop working.

Required fix:

- Merge fields individually when overriding a built-in.
- Use pointer fields or another presence-aware representation where an
  explicitly empty value must differ from an omitted value.
- Alternatively, document that built-in replacement is complete and reject
  incomplete replacements with an actionable configuration error.
- Add a behavioural test for a glyph-only override that proves detection and
  launch behaviour remain intact.

### 5. The new `command` field crosses a line protocol into shell source without validation

Locations:

- `zdevd/cmd/zdev-show/main.go:1036`
- `bin/zdev:220`

`zdev-show agents` emits `command` and `launch` as tab-separated text. The
shell consumer interpolates `command` directly into a generated
`command -v ...; then exec ...` script. Tabs, newlines, or shell syntax in the
field can change record boundaries or the generated shell program.

The configuration file is user-owned, so this is not a privilege-escalation
boundary: the same user can already configure an arbitrary launch command.
It is nevertheless a correctness and safety hazard because a malformed probe
value can execute unexpectedly or break every newly opened project pane.

Required fix:

- Validate `command` as one executable token with no whitespace or shell
  metacharacters.
- Reject invalid agent specifications while loading configuration, with the
  offending agent name in the error.
- Use a robust encoded protocol rather than unescaped tab/newline-delimited
  records if arbitrary values must be supported.
- Add tests covering spaces, tabs, newlines, semicolons, quotes, empty values,
  and wrapper-based launch commands.

## Verification and non-findings

- `make -C zdevd test` passed.
- The passing canonical gate included plist validation, architectural shell
  gates, `gofmt`, `go vet ./...`, and `go test -race -count=1 ./...`.
- The hub change preserves single-writer ownership and introduces no new
  off-goroutine state access.
- The hub change adds no new I/O to state derivation and changes no persisted
  or protocol state.
- The explicit `✳ Claude Code` idle exception preserves the existing
  no-false-wait behaviour.
- Mechanical diff checks found no focused tests, new suppression directives,
  or snapshot tests.
- No active Hunk review session was available.

## Relationship to the broader hardening brief

The three destructive-command findings are also captured in
`docs/adversarial-review-and-qa-plan.md`. This report records the repeat
audit independently so the exact current findings, severity, causal chains,
and verification evidence can be reviewed without relying on conversation
history.

## Remediation — 2026-08-17, same day

- **Findings 1–3 fixed** in commit `3572afe` ("zdev: fail-closed stream rm").
  Two validated segments, canonical (`pwd -P`) workspace/initiative/target
  resolution with strict-containment proof, marker-based identity
  (`.pay/stack.yml` naming exactly `<init>-<name>`, target must not be a
  repository, marker revalidated on the printed resolved path immediately
  before removal), and fail-closed git inspection (no upstream → HEAD must
  be reachable from a remote-tracking ref; merge/rebase in progress refuses;
  inspection failure refuses). Behaviourally verified against a scratch
  workspace: traversal, absolute, option-like, deep, ordinary-repo,
  marker-less, wrong-name-marker, and symlink-escape targets all refuse
  with nothing deleted; unpushed no-upstream refuses; a clean pushed stream
  removes exactly the resolved target.
- **Findings 4–5 fixed** in the working tree alongside the in-flight agent
  registry work they belong to (uncommitted at the time of writing):
  `EffectiveAgents` now merges overrides field by field (omitted fields
  keep the built-in's; an explicitly empty list clears — presence-aware via
  nil), `Load` rejects malformed specs with the agent named (`command` must
  be one executable token, `launch` single-line so the tab-separated
  record cannot split), and `bin/zdev`'s chain builder skips any
  non-token `command` as defense in depth. Behavioural tests added:
  `TestEffectiveAgents_PartialOverrideMerges` (glyph-only override keeps
  detection + launch) and `TestLoad_RejectsMalformedAgentCommand` (spaces,
  tabs, semicolons, quotes, leading dash, newline launch, wrapper launch).

## Tooling status — refreshed 2026-08-18

### Implemented

- Commit `91372fc` added a pinned QA tier:
  `make -C zdevd check` runs the canonical `test` target followed by
  `staticcheck@2025.1.1`.
- The same commit added `make -C zdevd vulncheck`, using pinned
  `govulncheck@v1.1.4`. It remains separate because it requires network access
  to `vuln.go.dev` and must fail visibly rather than silently skip offline.
- The canonical `make -C zdevd test` gate enforces platform unit/plist
  validation, the architectural shell gates, no `gofmt -l` output,
  `go vet ./...`, and `go test -race -count=1 ./...`.
- Agent configuration now has focused behavioural validation in the working
  tree: partial built-in overrides preserve omitted fields, malformed probe
  commands and multiline launch records are rejected, and the shell consumer
  independently rejects unsafe executable tokens.
- `stream rm` now has fail-closed path, identity, and Git-reachability guards
  in committed production code (`3572afe`).

### Implemented — second pass, 2026-08-18 (commit `144ff28`)

- CI's gate step now runs `make -C zdevd check` — pinned `staticcheck` is
  CI-enforced per push on both platform legs.
- `govulncheck` runs weekly (plus manual dispatch) via
  `.github/workflows/vulncheck.yml` — scheduled rather than per-push because
  it needs `vuln.go.dev` and must fail visibly, never skip silently offline.
- `scripts/test-stream-contract.sh` is the checked-in destructive-command
  contract suite for `stream rm`: 19 cases against a hermetic throwaway
  workspace (path traversal/validation ×10, identity refusals ×5, git
  fail-closed ×3, and the happy path proving exactly the printed resolved
  target is removed with every bystander intact). Wired into both CI legs.
- Disclosure controls, two of three layers: full-history secret scanning
  (`.github/workflows/secret-scan.yml`, gitleaks pinned to its commit SHA,
  push + weekly) and the PR-template synthetic-fixtures checkbox
  (`.github/pull_request_template.md`).

### Implemented — third pass, 2026-08-18 (commit `7da245a`)

- `stream add` is now TRANSACTIONAL in production code: every mutating step
  routes failure through a rollback that removes the whole partial folder
  (which would otherwise row as a phantom stream home), INT/TERM during the
  build trigger the same rollback, `checkout -b` failure is finally checked,
  and add gains rm's segment validation (initiative, stream, and repo names;
  `--branch` requires a value) — closing the same traversal class finding 1
  covered for rm.
- The contract suite covers add: 14 cases (nine malformed-input refusals
  proving nothing is created; dead-source, second-repo, and missing-branch
  failures proving no partial folder survives; the happy path; and the
  add→rm round trip proving rm removes exactly what add builds). 33 cases
  total, in both CI legs.

### Not yet implemented

- The employer/internal-identifier scan (disclosure layer 2) — deliberately
  held: naming the identifiers in a public workflow repeats the disclosure;
  blocked on the decision of where the private denylist lives.
- `CODEOWNERS` — low value for a single-owner repo; revisit if contributors
  arrive.

### Current conclusion

All five findings are fixed (1–3 committed, 4–5 in the working tree with the
registry work they belong to), and every tooling follow-up that did not need
a policy decision has landed: CI-enforced static analysis, a scheduled
vulnerability scan, the full destructive-command regression fence for both
stream verbs, transactional stream add, and secret scanning. The only open
item is the identifier-scan denylist location — a policy call, not code.
