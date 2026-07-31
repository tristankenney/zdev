---
description: Fleet status — every project row, states, and provenance
---
Run `zdev-show list --json` and `zdev --list-projects -v` (Bash). Present the
fleet grouped as the sidebar does (alphabetical; groups and singles
interleaved, members under their group):
state per row, anything waiting/dead first, and note rows whose provenance is
unexpected. End with the footer-style tally.
