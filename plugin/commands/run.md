---
description: Spawn a supervised agent loop in a project
argument-hint: "<project> \"<prompt>\" [--name NAME] [--switch]"
---
Run `zdev run $ARGUMENTS` (Bash). This is the actuator: it ensures the
project's session exists, opens a new window running claude seeded with the
prompt (unattended: permissions skipped), and leaves it under zdev
supervision — sidebar, triage, and death detection all apply. Report the
window it landed in. Background by default; --switch jumps to it. If the
project is unknown, show `zdev --list-projects -v` and ask.
