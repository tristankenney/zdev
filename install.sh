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

echo ""
echo "Done. Toggle a sidebar pane to verify: zdev-sidebar-toggle"
