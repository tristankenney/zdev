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

echo "==> Building zdevd + installing launchd jobs"
make -C "$REPO/zdevd" install

echo ""
echo "Done. Toggle a sidebar pane to verify: zdev-sidebar-toggle"
