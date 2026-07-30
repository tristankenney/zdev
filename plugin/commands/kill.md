---
description: Kill project sessions
argument-hint: "<project>... | all | --idle [hours]"
---
Run `zdev kill $ARGUMENTS` (Bash). Confirm BEFORE killing anything that has
an agent in waiting or working state (check `zdev-show list --json` first) —
killing a session discards that agent's in-flight context. Rows persist as
absent; reopening is `zdev <project>`.
