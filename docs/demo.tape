# vhs tape — the README demo GIF (ROADMAP: `zdevd demo` → reproducible GIF).
#
# Renders the REAL sidebar (tea engine, rose-pine theme) against the fake
# fleet `zdevd demo` serves: a real-looking workspace (two initiative homes
# — marketplace, ai-at-pay — a folded projects/ drawer, and root singles
# zdev/dotfiles) through idle → work starts → a permission wait → the wait
# ages and a second decision wait joins → both escalate to urgent → a
# member dies → recovery. ~28s of scenario. Regenerate with:
#
#   make -C zdevd demo-gif        (starts the demo server, runs vhs, cleans up)
#
# Requires: vhs (brew install vhs) and an installed zdev (~/.local/bin).
# The demo server must already be listening on /tmp/zdev-demo.sock — the
# make target owns that lifecycle so the tape stays a pure recording script.

Output docs/demo.gif

Set FontSize 13
Set Width 640
Set Height 360
Set Padding 12
Set Theme "rose-pine-moon"

Hide
# TypingSpeed: the Hide-phase Type below is a long env-var command; at
# vhs's default ~50ms/keystroke it burns 8-10+ REAL seconds typing before
# Show ever starts recording — and the demo server's frame clock starts
# ticking the moment `zdevd demo` launches (Makefile, well before vhs), so
# that typing time silently eats the early "quiet fleet"/"work starts"
# frames before a single frame is ever shown on camera (caught 2026-08-02:
# the recording opened mid-escalation, never showing frame 0 or 1). Typing
# near-instantly keeps the visible scenario aligned with the fixtures'
# delay_seconds.
Set TypingSpeed 1ms
# env -u TMUX -u TMUX_PANE: vhs inherits the invoking shell's tmux env;
# without the scrub the renderer self-tags the OPERATOR'S pane as a
# sidebar and stamps render timestamps onto it (bitten 2026-08-02).
# ZDEV_SIDEBAR_GROUP=collapse: initiative homes render as headers and the
# projects/ drawer folds behind a rollup count. ZDEV_SIDEBAR_INITIATIVE=1
# and ZDEV_SIDEBAR_TRIAGE=1: the initiative home metadata rows and the
# ranked triage strip, both off by default, opted into for the recording.
Type `env -u TMUX -u TMUX_PANE ZDEVD_SOCKET=/tmp/zdev-demo.sock ZDEV_SIDEBAR_ENGINE=tea ZDEV_SIDEBAR_THEME=rose-pine ZDEV_SIDEBAR_GROUP=collapse ZDEV_SIDEBAR_INITIATIVE=1 ZDEV_SIDEBAR_TRIAGE=1 $HOME/.local/bin/zdev-sidebar-render`
Enter
Show

# Let the full scenario play: idle(0) -8s-> working -4s-> first wait -5s->
# wait ages + 2nd wait -5s-> escalation -5s-> death -5s-> recovery, plus a
# beat on the final frame so the loop point reads calm before it cuts. The
# idle→working gap is padded (8s, not 4s) because the demo server's frame
# clock starts the moment `zdevd demo` launches (Makefile, before vhs even
# starts) — the fixed ~3-4s of Makefile/registration overhead before Show
# begins recording would otherwise eat the quiet-fleet opening entirely.
Sleep 36s
