---
name: initiative
description: >
  Create, extend, inspect, and wind down cross-project initiatives — directories of
  full clones under ~/workspace/initiatives that isolate one unit of value delivery
  (PayX Marketplace, ai-at-pay) from the canonical checkouts and from each other.
  Use when the user says start an initiative, add a repo to an initiative,
  initiative status, wind down / close out an initiative, or wants isolated
  cross-repo work that reintegrates via PRs over time.
license: MIT
metadata:
  author: tristankenney
  version: "3.0"
---

# Initiative

An **initiative** is the unit of value delivered. It spans repos, lives exactly as
long as delivery requires, and its scope drifts as work reveals itself. Layout:

```
~/workspace/
  projects/<repo>              # canonical checkouts: mainline, reviews, boring
  initiatives/                 # ONE git repo versioning all initiative metadata
    <name>/
      INITIATIVE.md            # intent, tracker links, decisions, outcome
      notes/                   # the initiative's knowledge base (never a row)
      <repo>/                  # FULL CLONE of projects/<repo>, branch <name>/<thing>
```

**The disk is the registry** (ZDEV_PROJECTS_DISCOVER=1): a repo is in scope iff
its clone exists in the initiative directory, and the daemon watches the
workspace — rows, sidebar groups (`╭─ <name> ──` in the initiative's color), and
the M-p switcher all follow the filesystem within seconds. There is **no config
to edit and no daemon to kick** for any verb below. `zdev --list-projects -v`
shows why every row exists (the audit trail for the implicit convention);
`~/.config/zdev/projects` holds only overrides — favorites (` *`) and
off-convention paths.

**Clones, not worktrees**: an initiative can check out mainline (a worktree
never could while canonical held the branch), and repo state is fully isolated —
agent git surgery in one initiative cannot touch canonical or a sibling. Local
clones hardlink objects (cheap); origin is re-pointed at the real remote. The
trade: a clone's mainline is only as fresh as its own last `git fetch`.

Canonical checkouts stay boring: mainline, reading, one-off work (reviewing a
colleague's branch needs no initiative). Never do initiative work in canonical.

## Verbs

### Start an initiative

1. Confirm the name (short, kebab-case — it's a path segment, a branch prefix,
   a sidebar header, and its color identity). Ask for a one-line intent and
   tracker link(s) if any.
2. `mkdir -p ~/workspace/initiatives/<name>/notes`
3. Write `INITIATIVE.md` (template below); commit in the initiatives repo.
4. Optional: `bd init` in the initiative directory (beads — the agent-native
   work graph; see "Work items" below).
5. Only add clones for repos with work starting NOW — scope reveals itself
   through work; do not pre-provision the imagined full scope.

The sidebar group appears on its own.

### Add a repo

```bash
git clone ~/workspace/projects/<repo> ~/workspace/initiatives/<name>/<repo>
git -C ~/workspace/initiatives/<name>/<repo> remote set-url origin <github-url>
git -C ~/workspace/initiatives/<name>/<repo> checkout -b <name>/<thing>
```

That's the whole operation — the row appears within seconds.

### Status

- `zdev-show review --json` filtered to `initiatives/<name>/` projects — ready
  to land vs rotting.
- Per clone: `git log --oneline @{upstream}..` (unpushed); `git fetch -q &&
  git rev-list --count HEAD..origin/HEAD` (drift behind mainline).
- **Rot check**: a clone whose branch merged more than ~2 weeks ago still
  existing is rot — flag it, don't nurse it.

### Remove a repo / wind down

```bash
rm -rf ~/workspace/initiatives/<name>/<repo>    # full clone; nothing shared
```

Row disappears on its own. Wind-down = remove the last clone, fill
INITIATIVE.md's **Outcome**, commit, delete the directory — the initiatives
repo's history is the journal that outlives it.

## Work items (beads)

If the initiative runs agents that need shared, durable work state, use
**beads** (`bd`) rather than TODO lists in notes/ — dependency graph, `bd ready`
for unblocked work, atomic `--claim`, cross-session memory. `bd init` in the
initiative directory; agents working in the clones point at it with
`BEADS_DIR=~/workspace/initiatives/<name>/.beads`. INITIATIVE.md links the
tracker (Jira) above; bd is agent working memory below it. Don't hand-roll
either layer in markdown.

## INITIATIVE.md template

Holds only what CANNOT be derived from the directory: intent and links. Never
list member repos — the clones are the member list.

```markdown
# <name>

**Intent:** <one sentence — the unit of value this delivers>
**Started:** <date>
**Tracker:** <Jira/Linear links>

## Decisions
<!-- dated one-liners as they happen -->

## Outcome
<!-- filled at wind-down: what landed, where; what was abandoned, why -->
```

## What NOT to build into this

- No cross-initiative dependency tracking (tracker territory).
- No pre-registration of "planned" repos — a planned-but-absent repo is a
  tracker ticket, not a clone.
- No task lists in notes/ — that's bd's job (or the tracker's).
