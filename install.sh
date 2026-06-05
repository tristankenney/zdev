#!/usr/bin/env bash
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

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
# Go version floor: 1.23 (driven by fsnotify v1.10 + log/slog + math/rand/v2).
# Ubuntu apt's golang-go is 1.18–1.22 on every current LTS, which fails to
# compile with a cryptic "package log/slog is not in GOROOT" error halfway
# through `make install`. Detect early and point users at a working install.
go_raw=$(go version 2>/dev/null | awk '{print $3}' | sed 's/^go//')
go_major=${go_raw%%.*}
go_minor=${go_raw#*.}
go_minor=${go_minor%%.*}
if [[ -z "$go_major" || "$go_major" -lt 1 ]] || \
   { [[ "$go_major" -eq 1 ]] && [[ -z "$go_minor" || "$go_minor" -lt 23 ]]; }; then
  err_ "Go ${go_raw:-<unknown>} detected — zdev requires Go 1.23+"
  dim_ "(uses log/slog, math/rand/v2, and fsnotify v1.10 which need ≥1.23)"
  case "$OS" in
    Darwin)
      dim_ "macOS: brew install go    # currently ships 1.23+"
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
  next_step "Your tmux conf predates the bundled one — compare against $REPO/config/zdev.tmux.conf for new bindings (M-n next, M-a triage, M-r ack-all, M-? help)."
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
dim_ "keys          M-n next · M-a triage · M-r ack-all · M-? help/legend"

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
  "$C_BOLD" "$i" "$C_RST" "${ZDEV_SIDEBAR_THRESHOLD:-200}"
