#!/usr/bin/env bash
set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

symlink() {
  local src="$1" dst="$2"
  if [[ -L "$dst" ]]; then
    rm "$dst"
  elif [[ -e "$dst" ]]; then
    echo "SKIP $dst (exists and is not a symlink — move it manually)"
    return
  fi
  ln -s "$src" "$dst"
  echo "  $dst -> $src"
}

echo "==> Checking prerequisites"
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
    echo "  ERROR: unsupported OS \"$OS\". zdev supports macOS and Linux." >&2
    exit 1
    ;;
esac
for tool in "${common_tools[@]}" "${platform_tools[@]}"; do
  command -v "$tool" >/dev/null 2>&1 || missing+=("$tool")
done
if (( ${#missing[@]} > 0 )); then
  echo "  MISSING required tools: ${missing[*]}" >&2
  case "$OS" in
    Darwin)
      echo "  Install via Homebrew: brew install ${missing[*]}" >&2
      echo "  (plutil ships with macOS; launchctl too — re-run if these still show missing.)" >&2
      ;;
    Linux)
      echo "  Install via your distro's package manager. Examples:" >&2
      echo "    Debian/Ubuntu: sudo apt install ${missing[*]}" >&2
      echo "    Fedora/RHEL:   sudo dnf install ${missing[*]}" >&2
      echo "    Arch:          sudo pacman -S ${missing[*]}" >&2
      ;;
  esac
  exit 1
fi
tmux_major=$(tmux -V 2>/dev/null | awk '{print $2}' | cut -d. -f1)
if [[ -z "$tmux_major" || "$tmux_major" -lt 3 ]]; then
  echo "  WARNING: tmux $(tmux -V 2>/dev/null) detected — zdev requires 3.x with control mode" >&2
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
  echo "  ERROR: Go ${go_raw:-<unknown>} detected — zdev requires Go 1.23+" >&2
  echo "         (uses log/slog, math/rand/v2, and fsnotify v1.10 which need ≥1.23)" >&2
  case "$OS" in
    Darwin)
      echo "         macOS: brew install go    # currently ships 1.23+" >&2
      ;;
    Linux)
      echo "         Ubuntu apt's golang-go is too old on every current LTS." >&2
      echo "         Pick one of:" >&2
      echo "           sudo snap install go --classic" >&2
      echo "           sudo add-apt-repository -y ppa:longsleep/golang-backports && sudo apt update && sudo apt install -y golang-go" >&2
      echo "           # or download from https://go.dev/dl/ and add /usr/local/go/bin to PATH" >&2
      ;;
  esac
  exit 1
fi
if command -v gh >/dev/null 2>&1; then
  if ! gh auth status >/dev/null 2>&1; then
    echo "  NOTE: 'gh' is installed but not authenticated — PR probes will fail until you run 'gh auth login'"
  fi
else
  echo "  NOTE: 'gh' (GitHub CLI) not installed — PR probes will be no-ops"
fi
if ! command -v claude >/dev/null 2>&1; then
  echo "  NOTE: 'claude' not on PATH — sessions started by 'zdev <project>' will be shell-only."
  echo "        Override agent pane via ZDEV_AGENT_CMD if you use a different tool."
fi

echo "==> Shell scripts"
mkdir -p "$HOME/.local/bin"
for f in "$REPO/bin"/*; do
  name="$(basename "$f")"
  symlink "$f" "$HOME/.local/bin/$name"
done

echo "==> Personal config (~/.config/zdev/projects)"
mkdir -p "$HOME/.config/zdev"
if [[ ! -f "$HOME/.config/zdev/projects" ]]; then
  cp "$REPO/config/projects.example" "$HOME/.config/zdev/projects"
  echo "  created ~/.config/zdev/projects from projects.example"
else
  echo "  ~/.config/zdev/projects exists — leaving it alone"
fi

if [[ "$OS" == "Darwin" ]]; then
  echo "==> Building zdevd + installing launchd jobs"
else
  echo "==> Building zdevd + installing systemd user units"
fi
make -C "$REPO/zdevd" install

echo "==> Agent attention hooks (zdev-notify channel)"
# Idempotent: only appends missing entries, backs up before writing,
# skips machines without the agent's config dir. Powers the sidebar
# waiting/finished states, the ⚡ permission triage class, and the
# wait-tier notifications.
"$HOME/.local/bin/zdev-install-hooks" || echo "  hook install skipped (see above) — re-run later: zdev-install-hooks"

echo "==> tmux integration (~/.tmux.conf)"
# Idempotent: source config/zdev.tmux.conf (sidebar hooks, pane-title
# settings, and the core bindings M-n next / M-a triage / M-r ack-all /
# M-? help) from the user's tmux conf. Three cases:
#   1. conf already sources zdev.tmux.conf        → nothing to do
#   2. conf wires zdev by hand (copied hooks)     → leave it alone; the
#      user owns their integration — just point at what's new
#   3. no zdev integration                        → append the source line
TMUX_CONF="$HOME/.tmux.conf"
if grep -q "zdev.tmux.conf" "$TMUX_CONF" 2>/dev/null; then
  echo "  ~/.tmux.conf already sources zdev.tmux.conf — nothing to do"
elif grep -q "zdev-sidebar-toggle" "$TMUX_CONF" 2>/dev/null; then
  echo "  ~/.tmux.conf integrates zdev manually — leaving it alone."
  echo "  Compare against config/zdev.tmux.conf for additions"
  echo "  (current core bindings: M-n next, M-a triage, M-r ack-all, M-? help)."
else
  {
    printf '\n# zdev — sidebar hooks + triage bindings (added by zdev install.sh)\n'
    printf 'source-file %s\n' "$REPO/config/zdev.tmux.conf"
  } >> "$TMUX_CONF"
  echo "  appended 'source-file $REPO/config/zdev.tmux.conf' to ~/.tmux.conf"
fi
# Reload the live server so the integration lands without a restart.
if tmux info >/dev/null 2>&1; then
  tmux source-file "$TMUX_CONF" 2>/dev/null \
    && echo "  reloaded running tmux server" \
    || echo "  WARNING: 'tmux source-file ~/.tmux.conf' failed — fix the conf and reload manually"
fi

echo ""
echo "Done. Toggle a sidebar pane to verify: zdev-sidebar-toggle"
