---
name: initiative
description: >
  Work with cross-project initiatives — start one, add a repo, check status,
  track work items, wind one down. An initiative is a workspace-root group
  marked by INITIATIVE.md, holding a full clone of each repo that one unit of
  value delivery touches. Use when the user says start an initiative, add a
  repo to an initiative, initiative status, or wind down / close out an
  initiative.
license: MIT
metadata:
  author: tristankenney
  version: "5.0"
---

# Initiative

An initiative lives at `~/workspace/<name>/`: `INITIATIVE.md` (the mark),
`notes/`, optionally `.beads/`, and one full clone per repo it touches, each
on a `<name>/<thing>` branch.

Every verb below is a plain filesystem operation — the disk is the registry,
so rows appear and disappear on their own within seconds. There is never
config to edit or a daemon to restart.

The workspace's own `CLAUDE.md` documents the structural model (groups,
markers, what marking changes) and loads automatically. Don't restate it.

## Start an initiative

1. Confirm the name: short, kebab-case. It becomes a path segment, a branch
   prefix, a sidebar row, and a colour identity.
2. Ask for a one-line intent and any tracker links.
3. `mkdir -p ~/workspace/<name>/notes`
4. Write `INITIATIVE.md` (template below) — this file is what makes the
   directory an initiative. Commit it in the journal (`git -C ~/workspace`).
5. Optional: `bd init` in the initiative directory.
6. Clone ONLY repos with work starting now. Scope reveals itself through the
   work; never pre-provision the imagined full scope.

## Add a repo

```bash
git clone ~/workspace/projects/<repo> ~/workspace/<name>/<repo>
git -C ~/workspace/<name>/<repo> remote set-url origin <github-url>
git -C ~/workspace/<name>/<repo> checkout -b <name>/<thing>
```

That is the whole operation. Cloning from the local `projects/` checkout is
a speed optimisation (hardlinked objects); `gh repo clone` straight in is
equivalent. Re-pointing origin matters — every zdev surface keys on the
remote.

Full clones, not worktrees: the initiative can hold mainline, and its git
state is isolated from canonical and from sibling initiatives. The trade is
that its mainline is only as fresh as its own last `git fetch`.

## Parallel workstreams

One repo, several concurrent efforts (a PR stack being shepherded, a
feature build, a spike) — one clone can't hold them, and each effort
usually needs its own RUNNING environment. A **workstream** is a child
FOLDER of the initiative holding one full clone per repo it needs —
exactly a pay-cli stack, so each stream is one runner: its own set of
containers, its own compose project, its own DNS namespace
(`<service>.<init>-<stream>.orb.local`), all up simultaneously with no
port conflicts.

```bash
zdev stream add <initiative> <name> <primary-repo> [repo...] [--branch <existing>]
zdev stream rm  <initiative>/<name>
zdev stream ls  <initiative>
```

`add` writes `.pay/stack.yml` with the QUALIFIED name `<init>-<name>`
(compose projects — and therefore DNS — unique by construction), clones
each repo (hardlinked from the initiative's own member or `projects/`, so
the cost is untracked files), branches the primary repo to
`<init>/<name>` (or checks out `--branch`, the PR-stack case), and leaves
supporting repos on their default branch — the same branch may appear in
any number of streams, which is why these are full clones, not worktrees
(that, and a worktree's `.git` pointer file breaks git inside docker
bind-mounts).

Rows appear as `<init>/<stream>/<repo>`; sessions and loops target them
like any member. Run the stream with `pay dev up` in its folder. `rm`
refuses while any repo inside is dirty or unpushed.

A stream whose purpose ended is rot, same discipline as members.

## Status

- `zdev initiatives [--json]` — the first read, always: every initiative's
  intent, dated decisions, member branches with dirty/unpushed state, bd
  work counts, and notes in one local-only call (no daemon, no network).
  The `--json` shape is a versioned contract — see
  `docs/initiatives-digest.md` in the zdev repo; never parse INITIATIVE.md
  or invoke `bd stats` yourself.
- `zdev-show review --json`, filtered to `<name>/` — what's ready to land.
- Deeper per-clone drill-down when the digest flags something:
  `git log --oneline @{upstream}..` for the unpushed commits themselves;
  `git fetch -q && git rev-list --count HEAD..origin/HEAD` for drift behind
  mainline (the digest never fetches, so its numbers are as fresh as the
  clone's last fetch).
- **Rot check**: a clone whose branch merged more than ~2 weeks ago is rot.
  Flag it for deletion rather than nursing it — the `initiative-compact`
  skill is the full pass (decision folding, notes cull, bd hygiene, rot
  removal) when the whole initiative needs a tidy, not just one clone.

## Work items

When agents need durable shared work state, use beads (`bd`) — never TODO
lists in `notes/`. `bd init` in the initiative directory; agents working
inside member clones point at it with `BEADS_DIR=~/workspace/<name>/.beads`.
`bd ready` lists unblocked work, `bd update <id> --claim` takes it.
INITIATIVE.md links the tracker above; bd is the working memory below it.

## Remove a repo / wind down

```bash
rm -rf ~/workspace/<name>/<repo>    # a full clone — nothing is shared
```

The row disappears on its own. To wind down: remove the last clone, fill in
INITIATIVE.md's **Outcome**, commit in the journal, then delete the
directory. The journal's history is what outlives it.

## INITIATIVE.md template

Holds only what cannot be derived from the directory. Never list member
repos — the clones are the member list.

```markdown
# <name>

**Intent:** <one sentence — the unit of value this delivers>
**Started:** <date>
**Tracker:** <Jira/Linear links>

## Decisions
<!-- dated one-liners, added as they happen -->

## Outcome
<!-- at wind-down: what landed and where; what was abandoned and why -->
```
