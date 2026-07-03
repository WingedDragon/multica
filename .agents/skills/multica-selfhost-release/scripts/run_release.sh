#!/usr/bin/env bash
set -euo pipefail

REPO="${MULTICA_REPO:-/Users/dong/.wtc/projects/multica}"
REMOTE_JUMP="${MULTICA_REMOTE_JUMP:-my-mini}"
REMOTE_HOST="${MULTICA_REMOTE_HOST:-dj}"
REMOTE_DIR="${MULTICA_REMOTE_DIR:-/home/ubuntu/apps/multica}"
REMOTE_NAME="${MULTICA_REMOTE_NAME:-wingeddragon}"

SKIP_DEPLOY="${MULTICA_SKIP_DEPLOY:-0}"
SKIP_PACKAGE="${MULTICA_SKIP_PACKAGE:-0}"
SKIP_INSTALL="${MULTICA_SKIP_INSTALL:-0}"
SKIP_CLI_INSTALL="${MULTICA_SKIP_CLI_INSTALL:-0}"

CLI_BIN="$REPO/server/bin/multica"

build_cli() {
  echo "==> Build multica CLI"
  local version commit date
  version="$(git describe --tags --always --dirty 2>/dev/null || echo dev)"
  commit="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
  date="$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
  (
    cd "$REPO/server"
    go build -ldflags "-X main.version=$version -X main.commit=$commit -X main.date=$date" -o bin/multica ./cmd/multica
  )
}

install_local_cli() {
  echo "==> Local CLI install: $HOME/.local/bin/multica"
  # Rebase note: Homebrew may shadow ~/.local/bin in interactive shells on
  # this machine. Keep the uninstall before copying the freshly built CLI.
  if command -v brew >/dev/null 2>&1 && brew list --formula multica >/dev/null 2>&1; then
    brew uninstall multica
  fi
  mkdir -p "$HOME/.local/bin"
  install -m 0755 "$CLI_BIN" "$HOME/.local/bin/multica"
  "$HOME/.local/bin/multica" version
}

run_my_mini_zsh() {
  local script="$1"
  # Rebase note: use zsh -lc for my-mini so Homebrew is discoverable even from
  # a non-login ssh command; this matches prior daemon/PATH recovery work.
  ssh -o RequestTTY=no "$REMOTE_JUMP" "zsh -lc $(printf '%q' "$script")"
}

install_my_mini_cli() {
  echo "==> my-mini CLI install: ~/.local/bin/multica"
  run_my_mini_zsh '
set -euo pipefail
mkdir -p "$HOME/.local/bin"
if command -v brew >/dev/null 2>&1 && brew list --formula multica >/dev/null 2>&1; then
  brew uninstall multica
fi
'
  remote_tmp=".local/bin/multica.upload.$$"
  scp -o RequestTTY=no "$CLI_BIN" "$REMOTE_JUMP:~/$remote_tmp"
  run_my_mini_zsh "chmod 0755 \"\$HOME/$remote_tmp\" && mv \"\$HOME/$remote_tmp\" \"\$HOME/.local/bin/multica\" && \"\$HOME/.local/bin/multica\" version"
}

cd "$REPO"

branch="$(git rev-parse --abbrev-ref HEAD)"
if [ "$branch" = "HEAD" ]; then
  echo "Refusing to release from detached HEAD" >&2
  exit 2
fi

if [ -n "$(git status --porcelain)" ]; then
  echo "Local worktree is dirty. Commit or discard changes before release." >&2
  git status --short --branch >&2
  exit 2
fi

echo "==> Local branch: $branch"
git fetch upstream main
git rebase upstream/main
git push --force-with-lease origin "$branch"

if [ "$SKIP_CLI_INSTALL" != "1" ]; then
  build_cli
  install_local_cli
  install_my_mini_cli
fi

if [ "$SKIP_DEPLOY" != "1" ]; then
  echo "==> Remote deploy: $REMOTE_JUMP -> $REMOTE_HOST:$REMOTE_DIR"
  remote_script='
set -euo pipefail
cd "$REMOTE_DIR"
git fetch "$REMOTE_NAME" "$BRANCH"
if [ "$(git rev-parse --abbrev-ref HEAD)" != "$BRANCH" ]; then
  git switch "$BRANCH" || git switch -c "$BRANCH" "$REMOTE_NAME/$BRANCH"
fi
if [ -n "$(git status --porcelain)" ]; then
  unexpected="$(git status --porcelain | grep -v "^ M apps/web/next-env.d.ts$" || true)"
  if [ -n "$unexpected" ]; then
    echo "Remote worktree has unexpected local changes:" >&2
    git status --short >&2
    exit 4
  fi
  git restore apps/web/next-env.d.ts
fi
if ! git merge --ff-only "$REMOTE_NAME/$BRANCH"; then
  git reset --hard "$REMOTE_NAME/$BRANCH"
fi
./scripts/deploy.sh
git status --short --branch
git rev-parse HEAD
systemctl is-active multica-backend multica-frontend
'
  ssh "$REMOTE_JUMP" "ssh $REMOTE_HOST 'REMOTE_DIR=$(printf '%q' "$REMOTE_DIR") REMOTE_NAME=$(printf '%q' "$REMOTE_NAME") BRANCH=$(printf '%q' "$branch") bash -s'" <<<"$remote_script"
fi

if [ "$SKIP_PACKAGE" != "1" ]; then
  echo "==> Local package"
  ./scripts/package.sh
fi

if [ "$SKIP_INSTALL" != "1" ]; then
  app_path="apps/desktop/dist/mac-arm64/Multica.app"
  if [ ! -d "$app_path" ]; then
    echo "Missing app bundle: $app_path" >&2
    exit 3
  fi
  echo "==> Replace /Applications/Multica.app"
  osascript -e 'tell application "Multica" to quit' >/dev/null 2>&1 || true
  for _ in $(seq 1 20); do
    if ! pgrep -f '/Applications/Multica.app/Contents/MacOS/Multica|/Applications/Multica.app/Contents/Frameworks/Multica Helper' >/dev/null; then
      break
    fi
    sleep 1
  done
  pkill -f '/Applications/Multica.app/Contents/MacOS/Multica|/Applications/Multica.app/Contents/Frameworks/Multica Helper' >/dev/null 2>&1 || true
  rm -rf /Applications/Multica.app
  ditto "$app_path" /Applications/Multica.app
  xattr -dr com.apple.quarantine /Applications/Multica.app 2>/dev/null || true
  /usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' /Applications/Multica.app/Contents/Info.plist
  open -a /Applications/Multica.app
fi

echo "==> Local runtime config"
cat "$HOME/.multica/desktop.json"
