# zdev adversarial review and QA hardening brief

## Objective

Clean up the repository's inconsistent and dead surfaces, then make its testing and QA process match the needs of a serious, growing project.

Treat this as an implementation brief, not a request for another high-level review. Confirm each causal chain before changing code, preserve documented compatibility intentionally, and keep the fast local feedback loop practical.

## Repository context

- Go module: `zdevd/`
- Shell entrypoints: `bin/` and `install.sh`
- Primary gate: `make -C zdevd test`
- CI: `.github/workflows/ci.yml`
- Protocol: `zdevd/internal/proto`
- State/concurrency invariants: `.claude/agents/hub-invariants-reviewer.md`
- Current protocol schema at review time: `phase4-v24`
- Reviewed: 2026-08-17 at commit b901f92 — the "at review time" facts below
  (schema, formatting state, test counts) are snapshots of that commit

Any work touching `zdevd/internal/hub`, `applyEvent`, snapshots, or persistence must be checked against the hub invariants document.

## Verified findings

### 1. Most advertised `sidebar.toml` settings are parsed but ignored

`zdevd/cmd/zdevd/main.go` loads `sidebar.toml`, but only the agent registry, unmanaged mode, and collapse settings are passed into runtime components. The TODO around lines 191–194 explicitly says the remaining settings are not wired.

The public example nevertheless advertises these settings as operational:

- `workspace`
- `width`
- `stale_seconds`
- `wait_warn_seconds`
- `wait_urgent_seconds`
- `ports_max`
- `default_branches`
- `default_shells`
- `pr_refresh_seconds`
- `git_floor_seconds`
- `claude_glyph`
- `pi_glyph`

Runtime behaviour is instead hard-coded in several places:

- Port cap: `zdevd/internal/probes/lsof.go`
- Default branches: `zdevd/internal/policy/branch.go`
- Wait thresholds: `zdevd/internal/render/theme.go`
- Probe cadences: `zdevd/cmd/zdevd/main.go`
- Default shells: `zdevd/internal/tmuxctl/title.go`

Note that part of this surface is documented intent, not merely abandonment: `applyEnvOverrides` in `zdevd/internal/config/config.go` states "The 8 cadence/threshold keys are intentionally TOML-only per D4-13 (calibration knobs that take effect via `launchctl kickstart -k`)" — the wire-or-remove decision should be made against D4-13, not assumed to be drift.

There is also direct drift, and the three shell lists disagree beyond `pi`: config default `{zsh,bash,sh,fish,claude,claude.exe,pi}` (config.go:175), runtime `{bash,zsh,sh,fish,dash,claude,claude.exe}` (title.go:79, `pi` removed 260519-hww), and the example advertises a third list `{bash,zsh,sh,fish,dash}` — and the example's list is the one users copy.

#### Required outcome

Choose one coherent contract for every setting:

1. Wire it end-to-end and test the resulting behaviour, or
2. Remove it from the public config surface, parser model, and tests until supported.

Do not retain silently accepted no-op settings. Prefer removing settings that no longer fit the architecture over adding plumbing solely to preserve an abandoned design.

#### Acceptance criteria

- Every key in `config/sidebar.toml.example` demonstrably affects runtime behaviour.
- Every field on `config.Config` has a production consumer, excluding deliberately transitional fields with a dated removal note.
- Tests assert observable behaviour, not merely successful TOML decoding.
- Documentation identifies whether a setting belongs to TOML or the environment and which process consumes it.

### 2. The documented formatting and vet gates do not exist

`CLAUDE.md` says `make -C zdevd test` includes gofmt and vet. `zdevd/Makefile` currently runs custom architecture gates and `go test -race`, but neither `gofmt` nor `go vet`.

The gap was demonstrated during review by an unformatted `focus.go` that the gate accepted (since formatted in commit a85905d); nothing prevents recurrence. `go vet ./...` passed during this review, but it is not enforced.

#### Required outcome

Make one target the authoritative local, pre-push, and CI gate. At minimum it must enforce:

- No `gofmt -l` output
- `go vet ./...`
- Existing architectural shell gates
- `go test -race -count=1 ./...`
- Platform unit/plist validation already present

Keep the documentation, Makefile help, pre-push hook, and CI invocation consistent with the real commands.

#### Acceptance criteria

- An intentionally unformatted Go file makes the canonical gate fail.
- A vet failure makes the canonical gate fail.
- CI and the installed pre-push hook continue to call the same target (both already run `make -C zdevd test`).
- `make -C zdevd test` is green on supported macOS and Linux environments.

### 3. The phase4-v8 agent compatibility projection is dead at phase4-v24

`Project.AgentClaude` and `Project.AgentPi` were retained as a one-release projection when `AgentStates` was introduced in phase4-v8. They are still:

- Declared in `zdevd/internal/proto/proto.go`
- Populated in `zdevd/internal/hub/agents.go`
- Copied into snapshots in `zdevd/internal/hub/snapshot.go`
- Compared in hub snapshot equality logic
- Read in hub production logic: `WaitContext` capture is gated on `pd.AgentClaude != "waiting" && pd.AgentPi != "waiting"` (`zdevd/internal/hub/state.go:1226`)
- Read in hub production logic: per-subscriber wait suppression uses `p.AgentClaude == "" && p.AgentPi == ""` as its trigger and clears both fields plus `Status` in the clone (`zdevd/internal/hub/hub.go:1469`, `1491-1495`)

Production render and show paths derive attention from `Project.Attention` (falling back to the legacy `Status` string), not from `AgentStates` — no code outside `internal/hub` and `internal/proto` references `AgentStates` at all. Clients enforce exact schema equality, so an old phase4-v7 client cannot consume a phase4-v24 snapshot regardless of the legacy fields.

#### Required outcome

Port the two live hub reads to `AgentStates` first — this is a behavioural rewrite in those two spots, not pure machinery deletion; note the current suppression never fires for agents other than claude/pi, so decide explicitly whether to preserve or fix that in the same change. Then remove the legacy projections and all associated state, snapshot, equality, and test machinery. Apply the repository's normal protocol-schema bump and migration/rebuild documentation rules.

#### Acceptance criteria

- No production references to `AgentClaude` or `AgentPi` remain.
- The legacy fields have no readers; agent attention flows through `AgentStates` into the derived `Attention` the renderer and CLI already consume.
- Protocol goldens and schema version are updated intentionally.
- Renderer, CLI, hub, and protocol tests pass.

### 4. Obsolete refresh scripts are still installed as public commands

`zdevd/scripts/check-no-shell-out.sh` says these scripts were subsumed by in-process probes and are obsolete:

- `bin/zdev-sidebar-pr-refresh`
- `bin/zdev-sidebar-ports-refresh`

`install.sh` symlinks every `bin/*` file into `~/.local/bin`, so both obsolete scripts are still distributed. `bin/zdev-doctor` also describes the PR-refresh script as if it powered the current chips.

#### Required outcome

Remove the obsolete scripts and update stale documentation/comments. Preserve any useful behavioural examples as Go fixtures or tests rather than installed executables.

#### Acceptance criteria

- Installation no longer exposes either obsolete command.
- No current operational documentation says the scripts power the sidebar.
- In-process GH and lsof probes retain equivalent test coverage.

### 5. A Git integration test inherits user configuration

`TestDeriveMember_LiveGit` in `zdevd/cmd/zdev-show/initiatives_test.go` creates a temporary repository but inherits global Git configuration. It failed during review because the user's `commit.gpgsign=true` caused Git to access an unavailable GPG setup.

The test passes when global Git config is suppressed.

#### Required outcome

Make Git-backed tests hermetic. Prefer a shared test helper that isolates global/system configuration and pins identity, locale, and other relevant environment values.

At minimum consider:

```text
GIT_CONFIG_GLOBAL=/dev/null
GIT_CONFIG_NOSYSTEM=1
```

Setting `git -c commit.gpgsign=false` fixes this one symptom but provides weaker isolation against future developer-specific configuration.

#### Acceptance criteria

- The test passes when the caller has commit signing enabled globally.
- Tests cannot read or mutate the developer's real Git configuration.
- Other tests that create Git repositories use the same isolation helper.

### 6. `zdev stream rm` can delete a non-stream repository without safety checks

The second audit found a data-loss path in `bin/zdev`'s stream removal command.

The stream identity check currently rejects the target only when BOTH of these are true:

- `.pay/stack.yml` is absent, and
- the target directory is absent.

That means any existing `<initiative>/<name>` directory is accepted, even when it is an
ordinary initiative member rather than a stream. Running:

```text
zdev stream rm marketplace/pay-app
```

against a normal `marketplace/pay-app` clone passes the identity check. The dirty/unpushed
loop examines repositories one directory BELOW the target, not the target repository
itself, so it sees none. The command then executes `rm -rf` on the ordinary clone.

The user-visible safety promise—“refuses if anything is dirty or unpushed”—does not protect
this case. This is merge-gating because a plausible typo or misunderstanding irreversibly
deletes a working copy without inspecting it.

There is a second escape: `stream_path` is only checked for containing `/`. `..` segments
are accepted and the joined path is never cleaned and proven to remain beneath the intended
initiative directory. A path such as `../some-directory` can target a sibling of the
workspace if it exists.

#### Required outcome

- Resolve the workspace and target to cleaned absolute paths.
- Reject absolute inputs, empty/dot segments, `..`, extra depth, and paths outside the
  workspace.
- Require exactly `<initiative>/<stream>` as two validated kebab-case path segments.
- Require the initiative marker and a valid stream marker/schema owned by zdev. Directory
  existence alone is not stream identity.
- Refuse deletion unless the resolved target is strictly below the resolved initiative
  directory and is neither the initiative nor workspace root.
- Re-resolve and print the exact target before the destructive step.

Prefer putting the destructive operation behind a small Go command/helper where path
containment, filesystem identity, and Git status can be tested portably. If it remains in
shell, centralise validation and add an exhaustive contract harness using a temporary
workspace.

#### Acceptance criteria

- Ordinary initiative member repos, initiative roots, the workspace root, siblings of the
  workspace, symlink escapes, `.`/`..`, absolute paths, and three-or-more-segment paths are
  all rejected without mutation.
- Removal succeeds only for a zdev-created stream carrying a valid marker.
- Tests verify the exact resolved deletion target before allowing cleanup.
- Failed validation and failed runner teardown leave every file intact.

### 7. Stream removal treats “no upstream” as “nothing unpushed”

The removal guard computes ahead count with:

```text
git rev-list --count @{upstream}..HEAD ... || echo 0
```

A branch with no upstream takes the error branch and becomes `0`. That is the unsafe answer:
unpushed-ness is unknown, not zero. It is especially likely for the primary branch created
by `zdev stream add`, because `checkout -b <initiative>/<stream>` creates it without an
upstream until the user explicitly pushes with `-u`.

Causal chain: create stream → commit locally on its new primary branch → do not push/set an
upstream → `stream rm` reports no unpushed commits → clone and commits are deleted.

#### Required outcome

Fail closed when upstream is absent. Require one of:

- A configured upstream with ahead count zero, or
- An explicit, separately confirmed override designed for knowingly disposable branches.

Also consider commits reachable only from the stream clone, detached HEADs, submodules, and
ignored-but-valuable generated state. The current status check correctly catches untracked
files but does not prove commit reachability elsewhere.

#### Acceptance criteria

- A branch with no upstream blocks removal.
- A detached HEAD blocks removal unless its commit is proven reachable from an allowed
  remote ref.
- Ahead, dirty, untracked, merge/rebase-in-progress, and submodule-dirty cases are tested.
- The error tells the user what must be pushed or preserved.

### 8. Stream creation is non-transactional and under-validates names/options

`stream add` writes the directory and `.pay/stack.yml` before validating that every requested
source repo can be cloned. A missing later repo, checkout collision, subcommand error, or
interruption leaves a partially-created directory that discovery can begin exposing as a
real stream. There is no rollback or incomplete-state marker.

The command says stream names must be kebab-case but merely rejects `/`, `.`, and `_`; spaces,
control characters, shell-sensitive characters, and leading/trailing hyphens remain valid.
Initiative and repository arguments are not validated as safe path segments. `--branch`
without a value can terminate under `set -u` after partial state has already been written.

#### Required outcome

- Parse and validate all arguments and source repositories before writing anything.
- Create in a unique temporary sibling directory, complete every clone/configuration step,
  then atomically rename into place.
- Clean the temporary directory on every failure/signal, without touching a pre-existing
  target.
- Use a strict, documented name grammar and reject path separators, traversal, whitespace,
  control characters, and option-like names.
- Treat source fetch/update failures deliberately; do not promise “born current” while
  swallowing the fast-forward failure.

#### Acceptance criteria

- Every injected failure point leaves no discoverable partial stream.
- Invalid or missing option values fail before filesystem mutation.
- Duplicate repo arguments and primary-repo inconsistencies are rejected clearly.
- Contract tests cover success, rollback, interruption, offline behaviour, and existing
  branch checkout.

### 9. Stream management has no destructive-command contract tests

No test harness currently exercises `zdev stream add`, `rm`, or `ls`. Go tests cover stream
discovery, sorting, digest projection, collapse, and rendering, but none covers the shell
commands that create and destroy the underlying directories and container volumes.

This is a tooling gap with direct causal impact: the two deletion defects above are simple
boolean/path cases that an isolated temporary-workspace test would have caught.

#### Required outcome

Add a shell contract suite with stubbed `git`, `docker`, and other external commands, plus a
small number of real-Git temporary-repository tests. Run it in both macOS and Linux CI.

The suite must record every attempted destructive target and fail if it leaves its allocated
temporary root. Do not test deletion safety against the developer's actual workspace.

#### Acceptance criteria

- CI invokes the stream command contract suite.
- Destructive targets are constrained to a test-owned temporary root.
- Tests cover the validation matrix from findings 6–8.
- Bash 3.2 compatibility is exercised on macOS.

## Internal-information disclosure audit

Treat this repository as public. Its README publishes a `raw.githubusercontent.com`
installer and public clone instructions. No company-internal identifier, workflow,
repository name, ticket, customer detail, or proprietary example should enter a tracked
file unless it is intentionally approved for public disclosure.

### Current-tree findings

The review found no apparent live credentials, private keys, employee email addresses,
customer records, or private company endpoints in tracked files. Placeholder notification
tokens in tests and examples are visibly synthetic.

It did find these Pay-specific disclosures:

- `plugin/skills/initiative-compact/SKILL.md` hard-codes the private-looking GitHub
  organisation slug in `gh pr list --repo payau/<repo> ...`.
- `bin/zdev-loop` uses `PAYX-70` as its example Jira-shaped ticket.
- `zdevd/cmd/zdevd/loops_test.go` repeats `PAYX-70` in test data and assertions.
- `zdevd/cmd/zdev-show/initiatives_test.go` contains the branded business phrase
  “payments land on Pay” in a product-shaped initiative fixture.

These strings were introduced in existing commits, not merely in the working tree. Their
current-tree removal will prevent further copying but will not erase them from clones,
forks, caches, or Git history.

The second audit found that the exposure is broader than those initial examples. Tracked
source, docs, demo fixtures, and plugin skills also include:

- Repository/product-shaped names including `pay-app`, `pay-id`, `pay-mailer`,
  `pay-toggles`, `pay-ops`, and `pay-cli`.
- The initiative name `ai-at-pay` and references to an “AI-at-Pay corpus” and internal
  research-note paths.
- Operational commands such as `pay dev up`.
- `.pay/stack.yml` conventions and statements about the manifest's service-mocking
  behaviour.
- Internal-looking DNS/compose conventions in the form
  `<service>.<initiative>-<stream>.localhost`.
- Workflow assertions about one runner, container/volume teardown, supporting repository
  topology, and how parallel stacks are named and launched.

Some of these details appear in a user-installable plugin that proactively instructs an AI
agent how and when to create Pay-shaped work environments. This is more than incidental
branding: in aggregate it describes employer-specific development topology and operational
practice. Whether each detail is confidential is a company-policy decision, but a personal
public repository should not make that decision implicitly.

The workstream commits made after the first audit introduced additional Pay-specific fixture
names, demonstrating that the repository currently has no effective disclosure ratchet.

### Required remediation

Replace employer-specific examples with neutral fixtures while preserving the behaviour
under test:

- `payau/<repo>` → `example-org/<repo>` or derive the owner from the clone remote.
- `PAYX-70` → a clearly fictional identifier such as `DEMO-70`.
- “payments land on Pay” → neutral wrapped prose with the same line-boundary properties.
- `pay-*` repository fixtures → neutral names such as `web-app`, `identity-service`, and
  `mailer`.
- `ai-at-pay` → a neutral initiative such as `agent-platform`.

Separate generic zdev workstream support from employer-specific runner integration. Public
core code and skills should describe a configurable runner seam rather than hard-code
`pay dev up`, `.pay/stack.yml`, private topology assumptions, or internal DNS naming. If a
Pay-specific adapter is useful, keep it in an approved private repository/package and make
the public integration generic.

Before rewriting Git history, establish whether the repository has public clones, forks,
or releases and follow the appropriate incident/disclosure process. History rewriting is
disruptive and cannot guarantee retraction. Do not undertake it as an ordinary cleanup
without explicit approval. If company policy treats the existing strings as sensitive,
escalate to the appropriate security or legal owner rather than making an ad hoc judgement.

### Disclosure controls to add

Add a lightweight public-repository safety gate that scans tracked content for known
employer identifiers and high-risk secret patterns. Keep the company-specific denylist in
a suitable private CI variable or approved repository policy if naming the identifiers in
the public scanner would itself repeat the disclosure. Note GitHub secrets are unavailable
to fork PRs on a public repo — the scan must fail loudly (or run on push/schedule) rather
than silently skipping when the denylist variable is absent.

At minimum, the process should cover:

- Employer names, organisation/repository slugs, internal domains, and ticket prefixes.
- Customer, merchant, employee, and partner names or identifiers.
- Production hostnames, account IDs, dashboards, Slack/Jira links, and incident IDs.
- API keys, bearer tokens, webhook URLs, credentials, private keys, and realistic secrets
  in fixtures.
- Proprietary code, schemas, runbooks, architecture details, metrics, commercial terms,
  and screenshots copied from internal systems.
- Meeting notes, chat excerpts, prompts, logs, stack traces, and test captures that may
  contain personal or operational data.

Use layered controls rather than relying on one regex:

1. A secret scanner over the full Git history in CI and before release.
2. A tracked-file disclosure scan for known internal terms.
3. A PR-template checkbox confirming that examples and fixtures are synthetic/public-safe.
4. `CODEOWNERS` or a required reviewer for changes to fixtures, docs, captures, plugins,
   and generated artefacts that commonly carry copied context.
5. Contributor guidance explaining that redaction means replacement with structurally
   equivalent synthetic data, not partial masking of real records.

When asking Claude or another AI system to work on this repository, provide only material
authorised for that specific system and account. Do not paste internal Pay code, tickets,
logs, customer data, credentials, or private links into prompts merely because the output
will eventually be sanitised. Confirm the applicable company AI/data-handling policy and
the environment's retention/training controls; this repository cannot enforce those
external guarantees itself.

### Disclosure acceptance criteria

- No tracked file contains the identified `payau`, `PAYX-70`, or Pay-branded fixture
  language unless an explicit public-disclosure decision is documented.
- Examples use obviously synthetic organisations, tickets, people, endpoints, and data.
- CI scans both secrets and known internal identifiers with an actionable failure message.
- The PR template asks authors to confirm that no internal or personal information was
  introduced.
- Release and documentation workflows scan generated HTML, images, recordings, fixtures,
  logs, and packaged plugin contents—not only Go and shell source.
- Any decision about existing Git-history exposure is recorded and owned by the appropriate
  security/legal authority.

## TUI-library leverage audit

The repository currently brings in:

- Bubble Tea `v1.3.10`
- Bubbles `v1.0.0`
- Lip Gloss `v1.1.0`
- BubbleZone `v1.0.0`
- `charmbracelet/x/ansi v0.11.6` transitively

The existing architecture already uses several of these well: Bubble Tea commands keep I/O
outside `Update`, Bubble Tea's line-diffing renderer materially reduces sidebar output
under the opt-in tea engine (`ZDEV_SIDEBAR_ENGINE=tea`; the default remains the pre-tea
classic loop, so most production sidebars run no Bubble Tea loop at all today),
BubbleZone keeps mouse hit-testing aligned with rendered rows, Bubbles provides the park
prompt's text input/help/key model, and the pinned Lip Gloss renderer prevents colour-profile
autodetection from changing output under tmux and tests.

Do not replace the renderer wholesale with generic Bubbles components. zdev has deliberate
protocol-driven ordering, exact ANSI/golden contracts, inline sidebar rendering, and
specialised animation semantics. Borrow focused primitives and patterns where they eliminate
known defects or duplicated machinery.

### T1. Replace byte/rune layout with terminal-cell-aware ANSI helpers

Several user-facing paths truncate with byte slices or count runes as if every rune occupied
one terminal cell:

- `cmd/zdev-round/round_view.go:114-117`
- `cmd/zdev-boundary/boundary_view.go:180-183`
- `cmd/zdev-show/main.go` preview and triage formatting
- `cmd/zdevd/loops.go` goal/session formatting
- `internal/render/width.go`

This can split a UTF-8 sequence into invalid output. Even the rune-safe helper in
`internal/render/width.go` mismeasures combining marks, emoji sequences, and double-width
CJK characters. A user-supplied project name, intent, wait summary, held title, branch, or
command can therefore corrupt a frame or push status columns and borders out of alignment.

`github.com/charmbracelet/x/ansi`, already in the dependency graph, provides:

- `ansi.StringWidth`: ANSI-aware terminal-cell width using grapheme clusters.
- `ansi.Truncate`: ANSI-safe, grapheme-safe, cell-width-aware truncation.
- `ansi.Cut`, `Hardwrap`, `Wordwrap`, and `Wrap` for related layout work.

Create one internal display-text package/helper and route every terminal width/truncation
decision through it. Preserve deliberate ASCII-only semantics only where input is actually
validated as ASCII.

Acceptance criteria:

- No user-controlled display string is truncated with a byte slice.
- Width tests cover CJK, combining accents, emoji, ZWJ emoji, embedded ANSI, and malformed
  UTF-8.
- Existing ASCII goldens remain unchanged unless an intentional layout improvement is
  documented.
- All rendered rows and borders stay within their assigned terminal-cell width.

### T2. Give Round and Boundary height-aware viewports

Both popup models accept `tea.WindowSizeMsg` but record only width. Both render every row and
clamp the cursor against the complete list. When the queue exceeds the popup height, the
selected row can move below the visible terminal region with no scroll offset or indication
that more content exists.

Borrow the state model from Bubbles `viewport`, `table`, or `paginator`:

- Track available body height from `WindowSizeMsg.Height` after subtracting header, footer,
  borders, and section separators.
- Maintain a visible offset and keep the cursor inside the viewport after keyboard, wheel,
  hover, refresh, defer, drop, and resize transitions.
- Show a quiet scroll affordance/count when rows exist above or below the viewport.
- Render and BubbleZone-mark only visible rows, then call `zone.Scan` at the final root.

Using `viewport.Model` directly is reasonable if its output composes cleanly with BubbleZone.
Otherwise copy its small offset/clamping pattern into the existing domain models. Do not feed
BubbleZone markers through a hard-trimming stage: BubbleZone's own documentation warns that
`MaxHeight`/`MaxWidth`-style clipping can cut markers before `Scan` sees them.

Acceptance criteria:

- The current row is always visible for queues larger than the terminal.
- Resize-to-smaller and resize-to-larger transitions preserve a valid selection.
- Mouse zones correspond only to visible rows and remain correct after scrolling.
- Tests cover section separators in Boundary because they consume height without mapping to
  a selectable row.

### T3. Match Bubble Tea renderer FPS to zdev's animation budget

Bubble Tea's standard renderer defaults to a 60 FPS ticker. The sidebar deliberately emits
animation messages at 15 FPS when waiting, 5 FPS when idle, and an effectively paused cadence
when invisible. Without `tea.WithFPS`, each tea-engine sidebar renderer still wakes at up
to 60 FPS to flush/check its buffer. This applies only under `ZDEV_SIDEBAR_ENGINE=tea`
(classic-loop sidebars have no Bubble Tea ticker), so coordinate the `WithFPS(15)` rollout
with the pending engine-default flip rather than treating it as a live fleet-wide cost.

Add `tea.WithFPS(15)` to the sidebar program and benchmark it against the current engine using
the repository's multi-sidebar workload. This should cut renderer wake-ups without reducing
visible fidelity because no sidebar state is designed to change faster than 15 FPS.

Consider a lower explicit cap for the event-driven popups too, but measure input latency first.
Do not enable `tea.WithANSICompressor`: Bubble Tea marks it deprecated and documents a
noticeable performance penalty.

Acceptance criteria:

- Sidebar animation cadence and visual goldens remain unchanged.
- CPU/wake-up and bytes-written measurements compare default 60 FPS with the proposed cap
  across one and many sidebars — extend the existing `make bench-idle` rig
  (`BENCH_IDLE_THRESHOLD`, D4-16-calibrated) rather than inventing a new one.
- Keyboard and mouse latency remain subjectively immediate and pass any existing latency
  budgets.

### T4. Make Boundary mutations acknowledge success and failure

Boundary's `D` path removes a held item from the UI optimistically and launches
`DialHeldRemove` as an asynchronous `tea.Cmd`. The completion message carries no result, and
the user can quit immediately after pressing `D`. The UI can therefore claim the item was
dropped even if the socket call failed or the process exited before it completed.

Borrow Bubble Tea's normal request/result pattern already used elsewhere:

- Carry the item ID and error/result in the completion message.
- Track pending mutations in the model.
- On success, commit the local removal.
- On failure, restore the row and show a compact status/error without destroying the rest of
  the review.
- Prevent or deliberately resolve quit while destructive mutations are pending. This can be
  handled in the model's quit branches or with `tea.WithFilter`; explicit model state is
  preferable because it is easier to test and explain.

Apply the same audit to jump, anchor, park, defer, and refresh commands, but do not add waiting
chrome to operations whose outcome is intentionally fire-and-forget.

Acceptance criteria:

- A failed held-item removal never disappears permanently from the visible model.
- Immediate quit cannot silently abandon a user-confirmed destructive operation.
- Pending, success, timeout, and failure transitions have deterministic model tests.

### T5. Reuse Bubbles key/help patterns across all interactive popups

Park already uses `key.Binding` and `help.Model`; Round and Boundary manually switch on key
strings and hand-build footer legends. The duplication has already produced slightly different
key vocabularies and makes help text easy to drift away from actual behaviour.

Define domain-specific key maps implementing Bubbles' `help.KeyMap`, then render compact/full
help through `help.Model`. Enable or disable bindings based on state—for example, `D` only on a
held row—and let the help view adapt to terminal width.

Keep domain transitions in the current models. The objective is one source of truth for key
matching and discoverability, not adopting the generic list component's opinions about
filtering, titles, pagination, or styling. Route any lipgloss styling the bubbles
components carry through the repo's single pinned ANSI256 renderer
(`zdevd/internal/render/lipgloss.go`, gate-enforced by
`scripts/check-no-lipgloss-scatter.sh`) — lipgloss's default renderer strips color the
moment output isn't a real tty, which is exactly the golden/tmux pitfall the convention
exists for; park is the precedent to copy.

Acceptance criteria:

- Footer help is generated from the same bindings used for dispatch.
- Disabled actions are neither accepted nor advertised.
- Narrow popups receive compact help without overflowing.
- Existing keyboard aliases remain supported unless intentionally deprecated.

### T6. Tighten BubbleZone ownership without discarding it

Round and Boundary each call `zone.NewGlobal()` from package `init`. This is acceptable for
separate short-lived binaries, but it creates process-global mutable state and an asynchronous
worker that tests must poll. BubbleZone also documents `Close` for programs whose UI can end
while the process remains alive.

Evaluate an injected `*zone.Manager` owned by each model/program. This would isolate tests,
make lifecycle explicit, and remove cross-model global state. Only take this change if the
result stays simple; the current binaries exit with their UI, so this is a maintainability
improvement rather than an urgent leak fix.

At minimum:

- Document why global ownership is safe for these executables.
- Ensure future in-process composition cannot call `NewGlobal` twice or retain stale zones.
- Add lifecycle/race coverage around refresh and resize while mouse events arrive.

### T7. Add TUI-specific performance and visual QA

Extend the QA programme with measurements that reflect what users perceive:

- Benchmark `View` and `Update` for 10, 50, and 200 rows.
- Benchmark `zone.Scan` and mouse lookup separately.
- Track allocations per frame and bytes written per second for one and many sidebars.
- Test bursty snapshot input to ensure intermediate frames coalesce rather than backlog.
- Add fixed terminal-size goldens for narrow, normal, and wide layouts (the render
  package's `TestVisualParity` fixtures and `-update-render` flow are the vehicle).
- Add Unicode-heavy goldens and resize sequences.
- Exercise dark/light terminal assumptions, no-colour mode, ANSI256, and truecolour where
  those are supported contracts.
- Record a short deterministic animation fixture so pulse, spinner, breath, and celebration
  timing can be inspected rather than inferred from static frames.

Keep performance thresholds tolerant of shared CI hosts. Prefer regression ratios or generous
budgets over fragile microsecond absolutes.

### T8. Bound and sanitise synthetic group/stream headers

The new `writeStreamHeader` writes the stream name directly and does not receive the terminal
width. Existing group headers have the same underlying weakness. A long stream/group name can
wrap onto extra terminal lines even though the byte frame contains only one newline.

Causal chain: long directory/config name → synthetic header wraps visually → renderer and
BubbleZone row maps still count one logical line → every subsequent click/hover target is
shifted and the frame exceeds the pane width.

Names can originate from filesystem entries and project overrides. They must also be treated
as terminal input: control characters and ANSI/OSC sequences should not be allowed to become
terminal commands merely because they occur in a directory or configured name.

Pass the available width to every synthetic-header renderer, sanitise control sequences, and
truncate with the cell-aware helper from T1. Validate project/initiative/stream naming at
creation and config ingestion as defence in depth.

Acceptance criteria:

- Group and stream headers never occupy more than one terminal row at supported widths.
- Row maps remain correct after maximum-length headers.
- Control characters, ESC, newlines, tabs, bidi controls, and OSC-like input are rejected or
  rendered inert.
- Tests include narrow panes and adversarial names.

### T9. Compute frame closers in rendered order after demotion partitioning

`lastInGroup` and the new `lastInStream` arrays are computed in snapshot order. Fold-demotion
mode then renders active rows first, inserts a divider, resets group/stream header tracking,
and renders demoted rows afterward. A group or stream that straddles those partitions can
leave the active mini-frame open at the divider because its “last” row exists later in the
original snapshot, then re-open the same frame below the divider.

The existing comment accepts loose group restatement, but the nested stream rails make the
broken closure more visible and harder to parse. Compute closure flags over each actual
render-index sequence (`activeIdx` and `demotedIdx`) rather than once over snapshot order.

Acceptance criteria:

- Every visible group and stream frame closes before the demotion divider.
- The demoted block independently opens and closes any repeated group/stream.
- Tests cover one stream split across active/demoted rows, collapsed rows between visible
  members, current-session metadata, and the group's final stream.

### Recommended TUI implementation order

1. Cell-aware width/truncation helpers and Unicode regressions (T1).
2. Height-aware popup viewports and off-screen cursor tests (T2).
3. Boundary mutation result handling (T4).
4. `tea.WithFPS(15)` benchmark and rollout for the sidebar (T3).
5. Shared key maps and adaptive help (T5).
6. BubbleZone ownership cleanup if measurement/test isolation justifies it (T6).
7. Bound/sanitise synthetic headers and preserve row-map geometry (T8).
8. Compute group/stream frame closure in rendered partition order (T9).
9. Ongoing TUI benchmarks, resize tests, and visual fixtures (T7).

This order is independent of the main implementation sequence below — the TUI items touch
disjoint surfaces (popups and the tea engine) and can land in parallel with it or after
step 6, whichever review bandwidth allows.

## QA hardening programme

The repository already has a strong behavioural test base: roughly 1,000 Go tests, the race detector, macOS/Linux CI, protocol goldens, agent adapter contracts, a real tmux/daemon smoke test, specialised architectural gates, and a weekly OpenCode canary. Preserve those strengths.

The main weakness is fragmented enforcement. Implement the following in priority order.

### A. Establish explicit QA tiers

Provide clear targets with documented budgets:

- `make test`: fast, deterministic, hermetic, required before push.
- `make check`: full static analysis and broader verification suitable for CI (new).
- `make live-test`: keep the existing target — real tmux/socket/restart integration tests.

Avoid duplicating command lists across workflows. CI should call repository targets.

Suggested `check` additions:

- `staticcheck ./...`
- `shellcheck` for supported shell scripts
- Go vulnerability scanning (`govulncheck`)
- Coverage generation and reporting
- Existing adapter contract suites

Pin tool versions so local and CI results agree. If installing all tools makes the fast path too expensive, keep them in `check`, not `test`.

### B. Add coverage visibility before setting arbitrary gates

Produce per-package coverage in CI and retain the report as an artefact or job summary. Initially measure trends rather than enforcing a repository-wide percentage.

After a baseline is understood, set meaningful floors for critical packages such as:

- `internal/hub`
- `internal/socket`
- `internal/proto`
- `internal/tmuxctl`
- `internal/config`

Do not reward low-value line coverage. Prioritise state transitions, failure paths, protocol compatibility, retry/reconnect behaviour, and persistence migration.

### C. Add fuzzing at hostile-input boundaries

There were no Go fuzz tests at review time. Start with parsers and trust boundaries:

- tmux control-mode decoder/parser
- socket/protocol JSON messages
- persisted-state loading and migration
- notification payload parsing
- projects/config parsing
- event-log reading

Seed fuzzers with existing captures, protocol goldens, and malformed regression fixtures. Run a short fuzz budget in regular CI and longer jobs on a schedule.

### D. Schedule the live suite

Six live test files (plus the shared `internal/livehelpers` helper package) are protected by the `live` build tag and excluded from normal CI. Add a scheduled workflow that runs live tests on the platforms where their dependencies are meaningful.

The scheduled job should:

- Use explicit timeouts.
- Upload daemon logs, event logs, tmux captures, and diagnostics on failure.
- Avoid fixed sleeps where polling with a deadline is possible.
- Clean up only resources created by the job.

### E. Standardise hermetic subprocess tests

Create reusable helpers for subprocess-backed tests. They should provide isolated values for:

- `HOME`
- `XDG_CONFIG_HOME`, `XDG_STATE_HOME`, and `XDG_RUNTIME_DIR`
- `TMPDIR`
- `PATH`
- Git system/global configuration
- Locale and timezone where output parsing depends on them

Tests should never depend on the developer's signing keys, tmux server, Git aliases, shell startup files, or real zdev configuration unless explicitly tagged as live.

### F. Add dead-code and stale-surface checks

Add a pinned Go dead-code analyser to the comprehensive check. Also add a repository-specific validation for installed commands:

- Every file installed from `bin/` must be referenced by an active config, documented command, or explicit internal-tool allowlist.
- Deprecated compatibility code needs an owner/removal version or date.
- No public example may contain a parsed-but-unconsumed setting.

Keep this signal-oriented; do not create a large suppression baseline.

### G. Harden CI supply-chain configuration

GitHub Actions currently use mutable major tags such as `actions/checkout@v4`. Pin third-party actions to full commit SHAs and use Dependabot or Renovate to propose updates.

Give workflows the minimum required `permissions:` and set job-level `timeout-minutes` consistently.

## Recommended implementation sequence

Keep changes reviewable rather than landing one broad QA rewrite:

1. **Hermetic baseline:** fix Git test isolation, add gofmt/vet enforcement, and align documentation.
2. **Dead surfaces:** remove obsolete refresh scripts and stale doctor references.
3. **Disclosure remediation:** replace employer-specific fixtures with neutral ones and add the tracked-file disclosure scan (see the disclosure audit; history questions go to the designated owner, not this sequence).
4. **Protocol cleanup:** port the two live `AgentClaude`/`AgentPi` reads to `AgentStates`, then remove the fields, bump the schema, and update goldens.
5. **Configuration contract:** decide wire-or-remove per inert setting (against D4-13 where it applies), then implement behavioural tests.
6. **QA tiers:** introduce `check`, static analysis, coverage reporting, and pinned tool versions alongside the existing `test`/`live-test`.
7. **Continuous resilience:** add fuzzers and scheduled live/fuzz workflows.
8. **CI hardening:** pin actions and declare minimal permissions/timeouts.

Each step should be independently green and should not mix unrelated behavioural changes.

## Validation checklist

Run and report the results of all applicable checks (`check` exists only after sequence step 6; earlier steps validate with `test` and `live-test`):

```sh
make -C zdevd test
make -C zdevd check
make -C zdevd live-test
```

Also verify:

```sh
test -z "$(gofmt -l $(find zdevd -name '*.go'))"
cd zdevd && go vet ./...
```

For config work, add black-box or component-level tests proving that changing each retained setting changes observable behaviour. Parser-only tests are insufficient.

For protocol work, regenerate and inspect protocol/golden diffs and explain the compatibility story in the commit or PR description.

## Constraints

- Preserve the single-writer hub architecture.
- Keep `applyEvent` free of I/O.
- Thread time into derivation logic rather than calling `time.Now()` inside pure decisions.
- Do not weaken race testing or existing architecture gates to recover runtime.
- Do not add fixed sleeps to timing-sensitive tests.
- Do not silently retain no-op configuration for compatibility; either support it or reject/remove it clearly.
- Do not edit user-owned configuration or depend on the user's machine state in tests.

## Definition of done

This programme is complete when:

- The public configuration surface matches actual runtime behaviour.
- Transitional compatibility code and obsolete installed scripts are gone.
- The canonical local gate enforces everything the documentation claims.
- CI has fast, comprehensive, live, and scheduled resilience layers with clear ownership.
- Subprocess tests are hermetic.
- Coverage and fuzzing focus on critical behaviour and hostile inputs.
- All supported-platform CI jobs pass, with actionable diagnostics when they do not.
