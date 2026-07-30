---
description: Restart the zdev daemon and all sidebars
argument-hint: "[--build]"
---
Run `zdev restart $ARGUMENTS` (Bash; --build reinstalls from source first).
Afterwards verify: daemon pid via `launchctl list | grep com.zdev.zdevd`
(or systemctl --user on Linux) and a sidebar capture. Report what came back.
