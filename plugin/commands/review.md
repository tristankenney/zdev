---
description: Landing readiness — what is ready to ship, what is rotting
---
Run `zdev review --json` (Bash). Summarize per repo: ready-to-land count,
needs-a-fix, will-rot, and the longest-rotting age. Group initiative clones
under their initiative (the project path's first segment names it).
Recommend the single most valuable landing action.
