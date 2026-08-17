# The initiatives digest — `zdev initiatives [--json]`

`zdev initiatives` (a passthrough to `zdev-show initiatives`) emits a digest
of every initiative in the workspace: intent, dated decisions, member-clone
git state, bd work counts, and notes. `--json` is the machine contract this
document specifies; the default output is a compact human view of the same
data.

It exists so downstream consumers — Claude skills like `/plan`, scripts,
widgets — read initiative state through **one stable contract** instead of
each parsing `INITIATIVE.md` or invoking `bd` themselves. If you are writing
a consumer, depend on this document, not on the current implementation.

## Guarantees

- **Versioned contract.** Consumers may rely on every field documented here.
  Breaking changes (renaming a field, changing a field's type or semantics)
  bump the top-level `version`. Additive changes (new fields) do NOT bump it
  — consumers must ignore unknown fields.
- **Local-only.** The digest never touches the network: no `git fetch`, no
  API calls. Every member fact is derived from the clone's local state, so
  `unpushed` is relative to the last-fetched upstream ref and `bd` counts
  are whatever the local `.beads` database holds. Safe to call from hooks,
  skills, and offline sessions at any frequency.
- **No daemon.** Unlike most `zdev-show` subcommands this never dials the
  zdevd socket — it works on a machine where the daemon has never started.
- **Tolerant.** A missing or half-filled `INITIATIVE.md` section becomes
  `null`/empty, never an error. `bd` being uninstalled or failing makes
  `work` null; a corrupt member `.git` degrades that member's fields. The
  digest as a whole only fails (exit 1) when the workspace root itself is
  unreadable.
- **Empty is an answer.** A workspace with no initiatives yields
  `"initiatives": []` and exit 0.

## Discovery

An initiative is a directory **directly under the workspace root**
(`$ZDEV_WORKSPACE`, default `~/workspace`; gap-filled from
`~/.config/zdev/env`) that contains an `INITIATIVE.md` file. This mirrors
`bin/zdev`'s discovery rule exactly:

- a root directory **with `.git`** is a project, never an initiative — even
  if it happens to contain an `INITIATIVE.md`;
- hidden directories are skipped;
- unmarked groups (no `INITIATIVE.md`) are invisible to the digest.

Initiatives appear in directory-listing order (lexicographic).

## Shape (version 1)

```json
{
  "version": 1,
  "generatedAt": 1785796104,
  "workspace": "/Users/x/workspace",
  "initiatives": [
    {
      "name": "marketplace",
      "path": "/Users/x/workspace/marketplace",
      "intent": "Ship the PayX Marketplace MVP — …",
      "started": "2026-07-30",
      "tracker": "TBD — see notes/BREAKDOWN.md",
      "outcome": null,
      "decisions": {
        "count": 18,
        "latest": [ { "date": "2026-08-03", "text": "…" } ]
      },
      "members": [
        { "name": "pay-app", "branch": "marketplace/x",
          "dirty": false, "unpushed": 2, "lastCommitAt": 1785133283 }
      ],
      "work": { "tool": "bd", "open": 88, "ready": 52,
                "inProgress": 0, "blocked": 36 },
      "notes": ["BREAKDOWN.md"]
    }
  ]
}
```

### Top level

| field | type | meaning |
|---|---|---|
| `version` | int | Contract version. `1` for everything on this page. |
| `generatedAt` | int | Unix seconds at generation time. |
| `workspace` | string | Absolute workspace root the digest was derived from. |
| `initiatives` | array | One entry per discovered initiative; `[]` when none. |

### Initiative entry

All four `INITIATIVE.md`-derived string fields are `null` when the piece is
absent — "not filled in yet" is data, not an error.

| field | type | meaning |
|---|---|---|
| `name` | string | Directory basename. |
| `path` | string | Absolute path to the initiative directory. |
| `intent` | string \| null | Text of the `**Intent:**` line, prefix stripped. The value may wrap over multiple lines in the file (until the next bold field or a blank line); wrapped lines are joined with single spaces. |
| `started` | string \| null | Text of the `**Started:**` line, verbatim (usually `YYYY-MM-DD`, but not validated). |
| `tracker` | string \| null | Text of the `**Tracker:**` line, verbatim. |
| `outcome` | string \| null | Content under `## Outcome` with HTML comments stripped. `null` while only the template's `<!-- … -->` placeholder (or nothing) is there — i.e. `outcome != null` means the initiative has been (or is being) wound down. |
| `decisions` | object | See below. |
| `members` | array | See below; `[]` when the initiative has no clones. |
| `work` | object \| null | See below; `null` when there is no `.beads/`, bd is not installed, or bd failed. |
| `notes` | array of string | Filenames (not paths) of files directly in `notes/`, sorted; `[]` when the directory is absent or empty. Subdirectories are not listed. |

### `decisions`

Decisions are bullets under `## Decisions` matching `- YYYY-MM-DD — text`
(an em-dash or plain hyphen after the date). A bullet's indented following
lines — including nested sub-bullets — are joined into its text with single
spaces. Undated bullets are not decisions.

| field | type | meaning |
|---|---|---|
| `count` | int | Total number of dated decisions in the file. |
| `latest` | array | Up to the **5 most recent by date** (ties keep file order), **oldest-first** within the slice — print it verbatim and you get chronological order. `[]` when there are none. |

### Member entry

One per non-hidden child directory containing `.git` (file or directory —
clones or worktrees). Derived from local git state only.

| field | type | meaning |
|---|---|---|
| `name` | string | Directory basename. |
| `branch` | string | Current branch (`git rev-parse --abbrev-ref HEAD`); `"detached"` on a detached HEAD; `""` when underivable (corrupt `.git`). An unborn branch still reports its name. |
| `dirty` | bool | `git status --porcelain` is non-empty. |
| `unpushed` | int \| null | `git rev-list --count @{upstream}..HEAD`; `null` when the branch has no upstream (unknowable, distinct from a real `0`). Relative to the last local fetch — never refreshed by the digest. |
| `lastCommitAt` | int \| null | Unix seconds of the HEAD commit; `null` on an unborn HEAD. |

### `work`

Mapped from `bd stats --json` run with `BEADS_DIR=<initiative>/.beads` and
the initiative directory as cwd.

| field | type | meaning |
|---|---|---|
| `tool` | string | Always `"bd"` in version 1. |
| `open` | int | `summary.open_issues`. |
| `ready` | int | `summary.ready_issues` (unblocked, claimable). |
| `inProgress` | int | `summary.in_progress_issues`. |
| `blocked` | int | `summary.blocked_issues`. |

bd is an **optional dependency**: its absence or failure yields
`"work": null`, never a digest error. Consumers must treat `null` as "no
tracker signal", not "no work".

## Exit codes

- `0` — digest produced (including an empty one).
- `1` — the workspace root could not be read, or JSON marshalling failed.

## v1 additive: `members[].stream` (2026-08-17)

An initiative child folder without `.git` that holds repo clones is a
**workstream** (a pay-cli stack — one runner, one DNS namespace). Its repos
appear as members named `<stream>/<repo>` carrying `"stream": "<stream>"`;
direct members omit the field. The human render splits the count:
`2 members · 3 streams` (streams counted as folders, not repos).
