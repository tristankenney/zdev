#!/usr/bin/env bash
set -euo pipefail

# ---------- shell guard ----------
# This script is bash, not POSIX sh: arrays, [[ ]], process substitution.
# `sh install.sh` is the trap worth handling — macOS /bin/sh IS bash in POSIX
# mode, so it sets BASH_VERSION and happily runs two-thirds of an install
# (symlinks, workspace config, launchd jobs) before dying at the first
# `< <(...)` with a bare "syntax error near unexpected token `<'" and no hint
# that the shell was the problem. Detect POSIX mode via SHELLOPTS and re-exec
# under real bash rather than leaving a half-finished install behind.
zdev_needs_bash=no
[ -z "${BASH_VERSION:-}" ] && zdev_needs_bash=yes
case ":${SHELLOPTS:-}:" in *:posix:*) zdev_needs_bash=yes ;; esac
if [ "$zdev_needs_bash" = yes ]; then
  # Only re-exec when $0 is a real file. Piped invocations (`curl ... | sh`)
  # have nothing to re-exec, so say what to do instead of failing obscurely.
  if [ -r "$0" ]; then
    exec bash "$0" "$@"
  fi
  echo "install.sh: needs bash — re-run as 'bash install.sh' or 'curl -fsSL <url> | bash'" >&2
  exit 1
fi
unset zdev_needs_bash

# ---------- output helpers ----------
# Color only on a tty and when NO_COLOR is unset (https://no-color.org).
if [[ -t 1 && -z "${NO_COLOR:-}" ]]; then
  C_HEAD=$'\033[1;36m'; C_OK=$'\033[32m'; C_WARN=$'\033[1;33m'
  C_ERR=$'\033[1;31m'; C_DIM=$'\033[90m'; C_BOLD=$'\033[1m'; C_RST=$'\033[0m'
else
  C_HEAD=""; C_OK=""; C_WARN=""; C_ERR=""; C_DIM=""; C_BOLD=""; C_RST=""
fi
head_()  { printf '%s==> %s%s\n' "$C_HEAD" "$1" "$C_RST"; }
ok_()    { printf '  %s✓%s %s\n' "$C_OK" "$C_RST" "$1"; }
warn_()  { printf '  %s!%s %s\n' "$C_WARN" "$C_RST" "$1"; }
err_()   { printf '  %s✗ %s%s\n' "$C_ERR" "$1" "$C_RST" >&2; }
dim_()   { printf '  %s%s%s\n' "$C_DIM" "$1" "$C_RST"; }

# Next-step accumulator: anything the USER must do gets queued here and
# printed as a numbered list at the end — the install's last word is
# always "here is exactly what you do now, and where".
NEXT_STEPS=()
next_step() { NEXT_STEPS+=("$1"); }

symlink() {
  local src="$1" dst="$2"
  if [[ -L "$dst" ]]; then
    rm "$dst"
  elif [[ -e "$dst" ]]; then
    warn_ "SKIP $dst (exists and is not a symlink — move it manually)"
    return
  fi
  ln -s "$src" "$dst"
}

# ---------- self-bootstrap (curl -fsSL .../install.sh | bash) ----------
# When run from inside a checkout, REPO resolves to it and the install
# proceeds exactly as before. When run WITHOUT a checkout — piped from
# curl, or copied somewhere on its own — clone the repo and re-exec the
# cloned install.sh, so every install runs from a real checkout (the
# symlinks into ~/.local/bin and the tmux source-file line all point
# back into it; a checkout is not optional).
#
# Knobs (bootstrap only — no effect when already inside a checkout):
#   ZDEV_INSTALL_DIR   where to clone   (default: $ZDEV_WORKSPACE/zdev,
#                                        i.e. ~/workspace/zdev)
#   ZDEV_INSTALL_REPO  what to clone    (default: the GitHub repo)
looks_like_zdev_checkout() {
  # Structural check, not a path check: a dir is a zdev checkout iff it
  # has this script plus the two things the install wires up (bin/
  # scripts, the zdevd Go module). BASH_SOURCE alone can't be trusted —
  # a lone install.sh copied into ~/Downloads resolves fine but points
  # at nothing installable.
  [[ -f "$1/install.sh" && -f "$1/bin/zdev" && -d "$1/zdevd" ]]
}

REPO=""
_src="${BASH_SOURCE[0]:-}"
# Piped scripts leave BASH_SOURCE unset or pointing at a non-file
# ("bash", "/dev/stdin", ...); only trust it when it's a real file AND
# its directory passes the structural check.
if [[ -n "$_src" && -f "$_src" ]]; then
  _dir="$(cd "$(dirname "$_src")" && pwd)"
  looks_like_zdev_checkout "$_dir" && REPO="$_dir"
fi

if [[ -z "$REPO" ]]; then
  head_ "No zdev checkout here — bootstrapping one"
  INSTALL_DIR="${ZDEV_INSTALL_DIR:-${ZDEV_WORKSPACE:-$HOME/workspace}/zdev}"
  INSTALL_DIR="${INSTALL_DIR/#\~/$HOME}"
  INSTALL_REPO="${ZDEV_INSTALL_REPO:-https://github.com/tristankenney/zdev}"
  if looks_like_zdev_checkout "$INSTALL_DIR"; then
    # Idempotent re-run: an existing checkout is reused AS-IS — never
    # pulled or otherwise mutated behind the user's back.
    ok_ "existing checkout at $INSTALL_DIR — reusing it (not pulling)"
  elif [[ -e "$INSTALL_DIR" ]] && [[ -n "$(ls -A "$INSTALL_DIR" 2>/dev/null)" ]]; then
    err_ "$INSTALL_DIR exists and is NOT a zdev checkout — refusing to touch it"
    dim_ "move it aside, or point the install elsewhere:"
    dim_ "  ZDEV_INSTALL_DIR=~/somewhere/zdev  curl -fsSL .../install.sh | bash"
    exit 1
  else
    command -v git >/dev/null 2>&1 || {
      err_ "git is required to bootstrap (clone $INSTALL_REPO)"
      exit 1
    }
    dim_ "cloning $INSTALL_REPO → $INSTALL_DIR"
    mkdir -p "$(dirname "$INSTALL_DIR")"
    git clone -- "$INSTALL_REPO" "$INSTALL_DIR"
  fi
  # Re-exec the checkout's install.sh (arguments pass through). Piped
  # runs have the script itself on stdin — reattach the terminal when
  # there is one so the interactive prompts (workspace dir) still work.
  if [[ ! -t 0 ]] && { exec 3</dev/tty; } 2>/dev/null; then
    exec bash "$INSTALL_DIR/install.sh" "$@" <&3
  fi
  exec bash "$INSTALL_DIR/install.sh" "$@"
fi

head_ "Checking prerequisites"
OS="$(uname -s)"
missing=()
common_tools=(tmux go make jq)
case "$OS" in
  Darwin)
    platform_tools=(plutil launchctl)
    ;;
  Linux)
    platform_tools=(systemctl)
    ;;
  *)
    err_ "unsupported OS \"$OS\". zdev supports macOS and Linux."
    exit 1
    ;;
esac
for tool in "${common_tools[@]}" "${platform_tools[@]}"; do
  command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
done
if (( ${#missing[@]} > 0 )); then
  err_ "MISSING required tools: ${missing[*]}"
  case "$OS" in
    Darwin)
      dim_ "Install via Homebrew: brew install ${missing[*]}"
      dim_ "(plutil ships with macOS; launchctl too — re-run if these still show missing.)"
      ;;
    Linux)
      dim_ "Install via your distro's package manager. Examples:"
      dim_ "  Debian/Ubuntu: sudo apt install ${missing[*]}"
      dim_ "  Fedora/RHEL:   sudo dnf install ${missing[*]}"
      dim_ "  Arch:          sudo pacman -S ${missing[*]}"
      ;;
  esac
  exit 1
fi
ok_ "required tools present: ${common_tools[*]} ${platform_tools[*]}"
tmux_major=$(tmux -V 2>/dev/null | awk '{print $2}' | cut -d. -f1)
if [[ -z "$tmux_major" || "$tmux_major" -lt 3 ]]; then
  warn_ "tmux $(tmux -V 2>/dev/null) detected — zdev requires 3.x with control mode"
fi
# Go version floor: 1.24 (bubbletea's floor; previously 1.23 for fsnotify
# v1.10 + log/slog + math/rand/v2). Ubuntu apt's golang-go is years behind
# on every current LTS, which fails to compile with a cryptic "package … is
# not in GOROOT" error halfway through `make install`. Detect early and
# point users at a working install.
go_raw=$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')
go_major=${go_raw%%.*}
go_minor=${go_raw#*.}
go_minor=${go_minor%%.*}
if [[ -z "$go_major" || "$go_major" -lt 1 ]] || \
   { [[ "$go_major" -eq 1 ]] && [[ -z "$go_minor" || "$go_minor" -lt 24 ]]; }; then
  err_ "Go ${go_raw:-<unknown>} detected — zdev requires Go 1.24+"
  dim_ "(bubbletea needs ≥1.24; log/slog, math/rand/v2, fsnotify need ≥1.23)"
  case "$OS" in
    Darwin)
      dim_ "macOS: brew install go    # currently ships 1.24+"
      ;;
    Linux)
      dim_ "Ubuntu apt's golang-go is too old on every current LTS. Pick one of:"
      dim_ "  sudo snap install go --classic"
      dim_ "  sudo add-apt-repository -y ppa:longsleep/golang-backports && sudo apt update && sudo apt install -y golang-go"
      dim_ "  # or download from https://go.dev/dl/ and add /usr/local/go/bin to PATH"
      ;;
  esac
  exit 1
fi
ok_ "Go $go_raw"
if ! command -v fzf >/dev/null 2>&1; then
  warn_ "fzf not installed — the M-p project switcher and M-a triage popup won't open"
  case "$OS" in
    Darwin) next_step "Install fzf (powers the M-p switcher + M-a triage popup):  brew install fzf" ;;
    Linux)  next_step "Install fzf (powers the M-p switcher + M-a triage popup):  sudo apt install fzf" ;;
  esac
else
  ok_ "fzf present (M-p switcher, M-a triage popup)"
fi
if command -v gh >/dev/null 2>&1; then
  if ! gh auth status >/dev/null 2>&1; then
    warn_ "'gh' is installed but not authenticated — PR/CI chips stay empty"
    next_step "Authenticate the GitHub CLI so PR/CI chips populate:  gh auth login"
  else
    ok_ "gh authenticated"
  fi
else
  warn_ "'gh' (GitHub CLI) not installed — PR/CI chips will be no-ops"
  next_step "Optional: install the GitHub CLI ('gh') and run 'gh auth login' for PR/CI chips"
fi

head_ "Shell scripts → ~/.local/bin"
mkdir -p "$HOME/.local/bin"
n_scripts=0
for f in "$REPO/bin"/*; do
  name="$(basename "$f")"
  symlink "$f" "$HOME/.local/bin/$name"
  n_scripts=$((n_scripts + 1))
done
ok_ "$n_scripts scripts symlinked into ~/.local/bin"
case ":$PATH:" in
  *":$HOME/.local/bin:"*) ;;
  *)
    warn_ "~/.local/bin is NOT on your PATH — nothing zdev installs will be runnable"
    next_step "Add ~/.local/bin to PATH (then restart your shell):  echo 'export PATH=\"\$HOME/.local/bin:\$PATH\"' >> ~/.$(basename "${SHELL:-bash}")rc"
    ;;
esac

head_ "Workspace root"
# The one genuinely personal setting. Persisted to ~/.config/zdev/env,
# which the daemon and scripts read as a fallback when ZDEV_WORKSPACE
# isn't in their environment — launchd/systemd jobs don't inherit your
# shell env, so an export in .zshrc alone never reached the daemon.
mkdir -p "$HOME/.config/zdev"
ENV_FILE="$HOME/.config/zdev/env"
current_ws="${ZDEV_WORKSPACE:-}"
if [[ -z "$current_ws" && -f "$ENV_FILE" ]]; then
  current_ws=$(. "$ENV_FILE" 2>/dev/null; echo "${ZDEV_WORKSPACE:-}")
fi
default_ws="${current_ws:-$HOME/workspace}"
if [[ -t 0 ]]; then
  printf '  Where do your project checkouts live? %s[%s]%s ' "$C_DIM" "$default_ws" "$C_RST"
  read -r ws_answer || ws_answer=""
  ws="${ws_answer:-$default_ws}"
else
  ws="$default_ws"
  dim_ "non-interactive — using $ws"
fi
ws="${ws/#\~/$HOME}"
ws="${ws%/}"
if [[ ! -d "$ws" ]]; then
  warn_ "$ws does not exist yet"
  next_step "Create your workspace dir (and clone projects into it):  mkdir -p $ws"
fi
if [[ ! -f "$ENV_FILE" ]]; then
  printf '# zdev settings — sourced as fallback by zdevd and the zdev scripts.\n# Real environment variables win over entries here.\n' > "$ENV_FILE"
fi
if grep -q '^ZDEV_WORKSPACE=' "$ENV_FILE" 2>/dev/null; then
  sed -i.bak "s|^ZDEV_WORKSPACE=.*|ZDEV_WORKSPACE=$ws|" "$ENV_FILE" && rm -f "$ENV_FILE.bak"
else
  printf 'ZDEV_WORKSPACE=%s\n' "$ws" >> "$ENV_FILE"
fi
ok_ "workspace = $ws  (saved to ~/.config/zdev/env)"

head_ "Personal config"
if [[ ! -f "$HOME/.config/zdev/projects" ]]; then
  cp "$REPO/config/projects.example" "$HOME/.config/zdev/projects"
  ok_ "created ~/.config/zdev/projects (from projects.example — placeholder content)"
  next_step "Edit ~/.config/zdev/projects — one project per line, paths relative to $ws. Append ' *' to favorites."
else
  ok_ "~/.config/zdev/projects exists — left alone"
fi

# ---------- group layout (only when discovery is on) ----------
# ZDEV_PROJECTS_DISCOVER=1 makes the FLAT workspace layout the registry:
# groups are root dirs (INITIATIVE.md marks an initiative; unmarked =
# drawer), and the workspace root itself is the JOURNAL repo versioning
# every group's metadata via a whitelist gitignore. The journal's REMOTE
# is the user's call (it holds their notes) — queued as a next step.
# Knob off → this section is silent.
if grep -q '^ZDEV_PROJECTS_DISCOVER=1' "$HOME/.config/zdev/env" 2>/dev/null; then
  head_ "Group layout (discovery is on)"
  mkdir -p "$ws/projects"
  if [[ ! -d "$ws/.git" ]]; then
    git -C "$ws" init -q
    printf '/*\n!/.gitignore\n!*/\n/*/*\n!/*/INITIATIVE.md\n!/*/notes/\n!/*/notes/**\n!/*/AGENTS.md\n!/*/CLAUDE.md\n!/*/.beads/\n!/*/.beads/**\n' \
      > "$ws/.gitignore"
    git -C "$ws" add .gitignore
    git -C "$ws" commit -qm "workspace journal: scaffold" || true
    ok_ "initialized the workspace journal repo at $ws"
    next_step "Root repos with a top-level CLAUDE.md/AGENTS.md/notes need an exclusion line in $ws/.gitignore (e.g. /myrepo/) so the journal's metadata whitelist doesn't pierce into them."
  else
    ok_ "workspace journal repo exists — left alone"
  fi
  if ! git -C "$ws" remote get-url origin >/dev/null 2>&1; then
    next_step "Give the workspace journal a private remote (it holds your notes; commits are laptop-only until then):  gh repo create <you>/initiatives --private && git -C $ws remote add origin git@github.com:<you>/initiatives.git && git -C $ws push -u origin main"
  fi
fi

if [[ "$OS" == "Darwin" ]]; then
  head_ "Building zdevd + installing launchd jobs"
else
  head_ "Building zdevd + installing systemd user units"
fi
make -C "$REPO/zdevd" install
if [[ "$OS" == "Darwin" ]]; then
  ok_ "daemon installed (launchd label com.zdev.zdevd; logs: ~/Library/Logs/zdev/)"
else
  ok_ "daemon installed (systemd user unit zdevd.service)"
  dim_ "check it anytime:  systemctl --user status zdevd"
  linger_val=$(loginctl show-user "$USER" --property=Linger --value 2>/dev/null || true)
  if [[ "$linger_val" == "no" ]]; then
    next_step "Enable systemd linger so zdevd runs even when you're logged out: loginctl enable-linger $USER"
  fi
fi

head_ "Agent CLIs (sidebar agent pane + state detection)"
# Consult the registry — builtin agents plus any [[agent]] entries from
# sidebar.toml — rather than hardcoding one tool. `zdev <project>` opens
# an agent pane with the first of these found on PATH at session start.
agents_found=()
agents_missing=()
while IFS=$'\t' read -r binary launch; do
  [[ -z "$binary" ]] && continue
  if command -v "$binary" >/dev/null 2>&1; then
    agents_found+=("$binary")
  else
    agents_missing+=("$binary")
  fi
done < <("$HOME/.local/bin/zdev-show" agents 2>/dev/null || true)
if (( ${#agents_found[@]} > 0 )); then
  ok_ "found: ${agents_found[*]}${agents_missing[*]:+  (not installed: ${agents_missing[*]})}"
else
  warn_ "no supported agent CLI on PATH (looked for: ${agents_missing[*]:-claude opencode}) — sessions will be shell-only"
  next_step "Install an agent CLI (any of: ${agents_missing[*]:-claude opencode}), or set ZDEV_AGENT_CMD to your own. Custom agents: add [[agent]] entries in ~/.config/zdev/sidebar.toml (see config/sidebar.toml.example)."
fi

head_ "Agent attention hooks (zdev-notify channel)"
# Idempotent: only appends missing entries, backs up before writing,
# skips machines without the agent's config dir. Powers the sidebar
# waiting/finished states, the ⚡ permission triage class, and the
# wait-tier notifications.
if ! "$HOME/.local/bin/zdev-install-hooks"; then
  warn_ "hook install skipped (see above)"
  next_step "Re-run the agent hook installer once your agent's config dir exists:  zdev-install-hooks"
fi

head_ "tmux integration (~/.tmux.conf)"
# Idempotent: source config/zdev.tmux.conf (sidebar hooks, pane-title
# settings, and the core bindings M-n next / M-a triage / M-r ack-all /
# M-? help) from the user's tmux conf. Three cases:
#   1. conf already sources zdev.tmux.conf        → nothing to do
#   2. conf wires zdev by hand (copied hooks)     → leave it alone; the
#      user owns their integration — just point at what's new
#   3. no zdev integration                        → append the source line
TMUX_CONF="$HOME/.tmux.conf"
if grep -q "zdev.tmux.conf" "$TMUX_CONF" 2>/dev/null; then
  ok_ "~/.tmux.conf already sources zdev.tmux.conf"
elif grep -q "zdev-sidebar-toggle" "$TMUX_CONF" 2>/dev/null; then
  ok_ "~/.tmux.conf integrates zdev manually — left alone"
  next_step "Your tmux conf predates the bundled one — compare against $REPO/config/zdev.tmux.conf for new bindings (M-p switch, M-n next, M-a triage, M-r ack-all, M-? help)."
else
  {
    printf '\n# zdev — sidebar hooks + triage bindings (added by zdev install.sh)\n'
    printf 'source-file %s\n' "$REPO/config/zdev.tmux.conf"
  } >> "$TMUX_CONF"
  ok_ "appended 'source-file $REPO/config/zdev.tmux.conf' to ~/.tmux.conf"
fi
# Reload the live server so the integration lands without a restart.
if tmux info >/dev/null 2>&1; then
  tmux source-file "$TMUX_CONF" 2>/dev/null \
    && ok_ "reloaded running tmux server" \
    || { warn_ "'tmux source-file ~/.tmux.conf' failed"; next_step "Fix ~/.tmux.conf, then reload:  tmux source-file ~/.tmux.conf"; }
fi

# ---------- summary ----------
printf '\n%sInstalled%s\n' "$C_BOLD" "$C_RST"
dim_ "commands      ~/.local/bin/{zdev, zdev-show, zdevd, ...}"
dim_ "projects      ~/.config/zdev/projects"
dim_ "settings      ~/.config/zdev/env (ZDEV_WORKSPACE etc.)"
if [[ "$OS" == "Darwin" ]]; then
  dim_ "daemon        launchd com.zdev.zdevd (logs ~/Library/Logs/zdev/)"
else
  dim_ "daemon        systemd --user zdevd.service"
fi
dim_ "tmux conf     $REPO/config/zdev.tmux.conf"
dim_ "keys          M-p switch · M-n next · M-a triage · M-r ack-all · M-? help"

printf '\n%sWhat to do now%s\n' "$C_BOLD" "$C_RST"
i=1
for s in "${NEXT_STEPS[@]:-}"; do
  [[ -z "$s" ]] && continue
  printf '  %s%d.%s %s\n' "$C_BOLD" "$i" "$C_RST" "$s"
  i=$((i + 1))
done
printf '  %s%d.%s Start (or attach) tmux, then open a project:  zdev <project>\n' "$C_BOLD" "$i" "$C_RST"
i=$((i + 1))
printf '  %s%d.%s The sidebar appears on clients ≥%s cols wide (tune: ZDEV_SIDEBAR_THRESHOLD); press M-? for keys + glyph legend.\n' \
  "$C_BOLD" "$i" "$C_RST" "${ZDEV_SIDEBAR_THRESHOLD:-160}"
