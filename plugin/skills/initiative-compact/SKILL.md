---
name: initiative-compact
description: >
  Compact a living initiative: fold superseded decisions in INITIATIVE.md,
  cull dead notes, run bd hygiene, and remove rotted member clones — keeping
  the initiative lean without losing meaning. Use when the user says compact,
  clean up, tidy, or prune an initiative, or when INITIATIVE.md's decision log
  has grown past easy reading. For ending an initiative, use the initiative
  skill's wind-down instead.
license: MIT
metadata:
  author: tristankenney
  version: "1.0"
---

# Initiative compact

Compaction keeps a **living** initiative lean; it is not wind-down (that is
the `initiative` skill, and it fills **Outcome** — this skill never touches
Outcome). The confidence to delete comes from the workspace journal: every
INITIATIVE.md revision, culled note, and bd change is in `git -C ~/workspace`
history. **Git is the archive — delete, don't create `archive/` directories.**

Two invariants, checked before anything is written:

1. **The digest contract survives.** `zdev initiatives --json` parses
   `**Intent:**`, `- YYYY-MM-DD —` decision bullets, and `## Outcome`
   (contract: `docs/initiatives-digest.md`). Compaction rewrites content,
   never shape. Verify by running the digest after every INITIATIVE.md edit.
2. **Meaning survives.** Folding removes redundancy, not history's sense: a
   surviving decision absorbs what it superseded, by name. Someone reading
   only the compacted file must not be misled about what was decided.

## 1. Survey — read-only

```bash
zdev initiatives --json                      # orientation: decisions count, members, work
cat ~/workspace/<name>/INITIATIVE.md
ls ~/workspace/<name>/notes/
cd ~/workspace/<name> && bd stats            # open / blocked / closed shape
```

Per member clone (never trust memory over the disk):

```bash
git -C <clone> status --porcelain            # dirty?
git -C <clone> log --oneline @{upstream}.. 2>/dev/null   # unpushed?
git -C <clone> branch --show-current
gh pr list --repo payau/<repo> --head <branch> --state merged --json number,mergedAt
```

## 2. Propose — four lanes, then stop and agree

Present the plan as four lanes with concrete items in each. Destructive
classes (member deletion, bd prune) each need an explicit yes; INITIATIVE.md
folds can be agreed as a batch.

**Decisions.** A bullet is fold-able when a later decision supersedes it
("markdown-first breakdown" → "fanned out into beads"), when it became
standing structure (now in intent, a note, or AGENTS.md), or when it narrates
without deciding. Fold INTO the survivor: the surviving bullet gains
"(supersedes 2026-07-30 markdown-first)" or absorbs the dead bullet's
still-true content. Keep the survivor's original date and `- YYYY-MM-DD —`
shape. Living decisions — anything a newcomer still needs — stay verbatim.
Deferred/parked decisions (⏸) stay until resolved or explicitly dropped.

**Notes.** Cull what is actioned, superseded, or duplicated (a draft whose
questions were asked, a walkthrough folded into findings). Knowledge keeps:
anything still referenced by INITIATIVE.md, bd issues, or the notes index.
If `notes/README.md` indexes the set, update it to match what remains.

**Work items (bd).** Use bd's own hygiene, never hand-edit `.beads/`:

```bash
bd close <id...> --reason "…"     # stale/obsolete issues, reason per issue
bd prune                          # delete old closed beads (destructive — ask first)
bd gc                             # decay + Dolt compaction (ask first; bd restore undoes compaction, not prune)
```

Stale claims (`in_progress` with no matching activity in any clone) get
unclaimed or closed, not left lying.

**Members.** A clone is rot when its branch merged more than ~2 weeks ago
and nothing new started. Deletion gates — ALL must hold, verified fresh:
clean (`status --porcelain` empty), nothing unpushed, branch merged (or
never diverged from mainline). A dirty or unpushed clone is NEVER deleted —
flag it instead. Removal is `rm -rf ~/workspace/<name>/<repo>`; the sidebar
row disappears on its own.

## 3. Apply

Only what was agreed. Order: INITIATIVE.md → notes → bd → members — the
file edits are cheapest to re-verify, the member deletions are the least
reversible, so confidence builds in that order.

After the INITIATIVE.md rewrite:

```bash
zdev initiatives --json   # must still parse; intent, decision count, outcome as expected
```

## 4. Commit and report

Metadata changes are journal commits (clone removal needs none — the disk is
the registry):

```bash
git -C ~/workspace add <name>
git -C ~/workspace commit   # message: what was folded, culled, closed, removed — and why
```

Report before → after, one line per lane:

```
decisions 18 → 9 (7 folded into survivors, 2 became notes/DECISIONS-EARN.md)
notes     12 → 8 (4 culled: 2 actioned drafts, 1 duplicate, 1 superseded; index updated)
bd        88 open → 71 (14 closed with reasons, 3 unclaimed); prune deferred
members   11 → 9 (pay-tests, pay-renderer removed: merged 3+ weeks, clean, pushed)
```

## Edge cases

- **Nothing to compact** — say so and stop; a lean initiative is the goal,
  not a compaction commit.
- **Everything looks fold-able** — that's usually an initiative that should
  wind down instead; say so and point at the `initiative` skill.
- **A decision references a culled note** — either keep the note or fold the
  decision's still-relevant content inline before culling. Never leave a
  dangling reference.
- **bd verbs missing** (older bd) — close what you can, skip prune/gc, note
  the skip in the report.
- **Digest fails to parse after an edit** — restore from the journal
  (`git -C ~/workspace checkout -- <name>/INITIATIVE.md`), re-apply more
  conservatively. Never leave the file in a shape the digest can't read.
