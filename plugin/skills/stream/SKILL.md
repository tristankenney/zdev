---
name: stream
description: >
  Manage parallel workstreams inside a zdev initiative — create one, list
  them with runner state, tear one down. A workstream is a child folder of
  full clones (one pay-cli stack: its own containers and DNS), for the
  SECOND concurrent concern onward on the same repos. Use when the user
  says add/create/remove a stream or workstream, asks what streams exist,
  or wants parallel checkouts of a repo they're already working in. ALSO
  use proactively — OFFER a stream (never silently create one) when the
  user describes starting a second piece of work while the first is still
  in flight: waiting on a PR stack to merge and starting the next thing,
  spiking or exploring alongside an implementation, getting unblocked
  mid-day on a new thread that touches an already-busy repo. Do NOT
  trigger for: work that REPLACES the current work (just keep working on
  the floor), the first repo of an initiative (that is the initiative
  skill's add-a-repo), plain branch switching within one checkout, or
  repos in the projects/ drawer.
license: MIT
metadata:
  author: tristankenney
  version: "1.0"
---

# Workstreams

A **workstream** is a child folder of an initiative holding one full clone
per repo it needs. It is exactly one pay-cli stack: one runner, its own
compose project, its own DNS namespace
(`<service>.<initiative>-<stream>.orb.local`), running simultaneously with
every other stream. Streams pop in and out fast — a half-day spike is a
normal stream.

**The floor is stream zero.** An initiative's direct members are the
default workstream (runner: `pay dev up` at the initiative root). Streams
exist for the *second* concurrent concern onward. Never suggest moving the
floor's work into a stream folder — moving a directory destroys its tmux
session, renames its compose project, and orphans its per-directory Claude
conversation history.

## Commands

```bash
zdev stream add <initiative> <name> <primary-repo> [repo...] [--branch <existing>]
zdev stream ls  <initiative>
zdev stream rm  <initiative>/<name>
```

- `add`: fetches once at the clone source (streams are born current),
  branches the primary repo to `<initiative>/<name>` — or checks out
  `--branch` (shepherding an existing PR stack's top) — leaves supporting
  repos on their default branch, writes `.pay/stack.yml` with the
  globally-unique qualified name, and drops a CLAUDE.md into the stream
  folder that every agent inside inherits (runner location, BEADS_DIR,
  teardown command).
- `ls`: repos + branches + runner state per stream, and flags ORPHAN
  runners (compose projects with no surviving folder) with the teardown
  command.
- `rm`: tears down the runner (containers AND volumes) first; refuses
  while any repo inside is dirty or has unpushed commits. Remote branches
  survive.

Rows appear as `<initiative>/<stream>/<repo>` — sessions, loops
(`zdev run <initiative>/<stream>/<repo> … --until …`), and the sidebar all
follow the directory automatically.

## Offering a stream (the proactive half)

When the user describes parallel intent — "while that merges I want to
start X", "I had a chat that unblocked Y", "I want to explore Z on the
side" — and the work touches repos already active in an initiative:

1. Offer in ONE line with the exact command, e.g.:
   *"That's a second concurrent concern on pay-app — want a stream?
   `zdev stream add marketplace interface pay-app`"*
2. Name suggestion: short kebab-case after the CONCERN (backend,
   interface, spike-earn-selector), never after the ticket alone.
3. Include supporting repos only if the work runs them for real —
   pay-app's manifest mocks most services, so `pay-app` alone is the
   common case. Repos can be cloned into the stream folder later.
4. Create only on confirmation. Never create a stream as a side effect.

When work wraps ("that spike's done", "the stack merged"), offer the
matching `zdev stream rm` the same way — pop-out hygiene is part of the
model, and rm is guarded (dirty/unpushed refuse) so offering it is safe.

## Choosing floor vs stream

- First/main line of work → the floor (no ceremony)
- Second concurrent concern on the same repos → stream
- Different repos entirely, one concern → just add repos to the floor
- Violent spike (dependency surgery, history rewriting) → still a stream;
  full clones already isolate it completely
