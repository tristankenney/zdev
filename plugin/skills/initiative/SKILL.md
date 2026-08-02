---
name: initiative
description: >
  Create, extend, inspect, and wind down cross-project initiatives — marked
  groups of full clones at the workspace root that isolate one unit of value
  delivery (PayX Marketplace, ai-at-pay) from the canonical checkouts and from
  each other. Use when the user says start an initiative, add a repo to an
  initiative, initiative status, wind down / close out an initiative, or wants
  isolated cross-repo work that reintegrates via PRs over time.
license: MIT
metadata:
  author: tristankenney
  version: "4.0"
---

# Initiative

One structural concept exists: the **group** — a workspace-root directory of
members that folds behind a sidebar header. The tree mirrors the disk;
nothing about any path is special. Two questions decide the whole model:
a root dir **with `.git`** is a plain project, **without** it is a group;
a group **containing `INITIATIVE.md`** is an initiative, one **without** it
is a drawer. That file is the entire mark — `touch <group>/INITIATIVE.md`
promotes a drawer, deleting it demotes — never a config entry.

What the mark changes (grouping and folding work identically either way):
in a drawer EVERY child directory rows; in an initiative only children with
`.git` do, so `notes/` never becomes a row. A drawer's header is a dim
label; an initiative's own directory **is** a row (its "home"), carries the
initiative's identity colour, and can be entered with `zdev <name>` to work
at the initiative level. Only an initiative's metadata (`INITIATIVE.md`,
`notes/`, `AGENTS.md`, `.beads/`) is versioned by the workspace journal.

Mark a group when the work has an identity worth remembering; leave it a
drawer when it is just somewhere to keep checkouts (`projects/` is the
canonical drawer — standing checkouts, no metadata, no identity). An
initiative is the unit of value delivered: it spans repos, lives exactly as
long as delivery requires, and its scope drifts as work reveals itself.
Layout:

```
~/workspace/                   # ONE git repo (the journal) versioning all
  .gitignore                   #   group metadata via a whitelist
  projects/<repo>              # unmarked group: canonical checkouts
  <name>/                      # marked group = initiative
    INITIATIVE.md              # THE mark: intent, tracker links, decisions, outcome
    notes/                     # the initiative's knowledge base (never a row)
    <repo>/                    # FULL CLONE of projects/<repo>, branch <name>/<thing>
  <root-repo>/                 # dirs with .git at the root are plain rows (zdev, dotfiles)
```

The workspace journal needs a **private remote** (it holds your notes;
`zdev doctor` warns while commits are laptop-only — install.sh scaffolds the
repo and queues the remote as a next step). Push journal commits like any
other repo. Root repos with their own top-level CLAUDE.md/AGENTS.md/notes
need an exclusion line in the workspace .gitignore (e.g. `/zdev/`) so the
whitelist doesn't pierce into them.

**The disk is the registry** (ZDEV_PROJECTS_DISCOVER=1): a repo is in scope iff
its clone exists in the initiative directory, and the daemon watches the
workspace — rows, the sidebar group (`╭ <name>` home row in the initiative's
color), and the M-p switcher all follow the filesystem within seconds. There
is **no config to edit and no daemon to kick** for any verb below.
`zdev --list-projects -v` shows why every row exists (the audit trail for the
implicit convention); `~/.config/zdev/projects` holds only overrides —
favorites (` *`) and off-convention paths.

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
   a sidebar home row, and its color identity). Ask for a one-line intent and
   tracker link(s) if any.
2. `mkdir -p ~/workspace/<name>/notes`
3. Write `INITIATIVE.md` (template below) — this file IS what makes the group
   an initiative; commit in the workspace journal.
4. Optional: `bd init` in the initiative directory (beads — the agent-native
   work graph; see "Work items" below).
5. Only add clones for repos with work starting NOW — scope reveals itself
   through work; do not pre-provision the imagined full scope.

The sidebar group appears on its own.

### Add a repo

```bash
git clone ~/workspace/projects/<repo> ~/workspace/<name>/<repo>
git -C ~/workspace/<name>/<repo> remote set-url origin <github-url>
git -C ~/workspace/<name>/<repo> checkout -b <name>/<thing>
```

That's the whole operation — the row appears within seconds. The local
source is a creation-time optimization only (instant, hardlinked objects);
`gh repo clone` straight into the initiative works identically — after
origin points at GitHub the clone has NO ongoing relationship with the
canonical checkout, and every zdev surface (grouping, review gauge, rot)
keys on the remote. A canonical checkout only needs to exist if you
actually open it.

### Status

- `zdev-show review --json` filtered to `<name>/` projects — ready
  to land vs rotting.
- Per clone: `git log --oneline @{upstream}..` (unpushed); `git fetch -q &&
  git rev-list --count HEAD..origin/HEAD` (drift behind mainline).
- **Rot check**: a clone whose branch merged more than ~2 weeks ago still
  existing is rot — flag it, don't nurse it.

### Remove a repo / wind down

```bash
rm -rf ~/workspace/<name>/<repo>    # full clone; nothing shared
```

Row disappears on its own. Wind-down = remove the last clone, fill
INITIATIVE.md's **Outcome**, commit, delete the directory — the workspace
journal's history is the record that outlives it.

## Work items (beads)

If the initiative runs agents that need shared, durable work state, use
**beads** (`bd`) rather than TODO lists in notes/ — dependency graph, `bd ready`
for unblocked work, atomic `--claim`, cross-session memory. `bd init` in the
initiative directory; agents working in the clones point at it with
`BEADS_DIR=~/workspace/<name>/.beads`. INITIATIVE.md links the
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
