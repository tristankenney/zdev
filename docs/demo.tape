# vhs tape — the README demo GIF (ROADMAP: `zdevd demo` → reproducible GIF).
#
# Renders the REAL sidebar (tea engine, rose-pine theme) against the fake
# fleet `zdevd demo` serves: idle → working → waiting (tier escalation) →
# death, ~24s of scenario. Regenerate with:
#
#   make -C zdevd demo-gif        (starts the demo server, runs vhs, cleans up)
#
# Requires: vhs (brew install vhs) and an installed zdev (~/.local/bin).
# The demo server must already be listening on /tmp/zdev-demo.sock — the
# make target owns that lifecycle so the tape stays a pure recording script.

Output docs/demo.gif

Set FontSize 13
Set Width 460
Set Height 320
Set Padding 12
Set Theme "rose-pine-moon"

Hide
# env -u TMUX -u TMUX_PANE: vhs inherits the invoking shell's tmux env;
# without the scrub the renderer self-tags the OPERATOR'S pane as a
# sidebar and stamps render timestamps onto it (bitten 2026-08-02).
Type `env -u TMUX -u TMUX_PANE ZDEVD_SOCKET=/tmp/zdev-demo.sock ZDEV_SIDEBAR_ENGINE=tea ZDEV_SIDEBAR_THEME=rose-pine ZDEV_SIDEBAR_GROUP=collapse $HOME/.local/bin/zdev-sidebar-render`
Enter
Show

# Let the full scenario play: 8s idle→working + 8s →waiting + 8s →dead,
# plus a beat on the final frame so the death registers before the loop.
Sleep 27s
