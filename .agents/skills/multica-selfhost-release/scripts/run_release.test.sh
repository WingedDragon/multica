#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT

FAKE_BIN="$TMP/bin"
REPO="$TMP/repo"
HOME_DIR="$TMP/home"
LOG="$TMP/commands.log"

mkdir -p "$FAKE_BIN" "$REPO/server/bin" "$HOME_DIR/.multica"
printf '{}\n' >"$HOME_DIR/.multica/desktop.json"
: >"$LOG"

cat >"$FAKE_BIN/git" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
case "$*" in
  "rev-parse --abbrev-ref HEAD") echo "feature/selfhost-cli-update" ;;
  "status --porcelain") ;;
  "fetch upstream main") ;;
  "rebase upstream/main") ;;
  "push --force-with-lease origin feature/selfhost-cli-update") ;;
  "describe --tags --always --dirty") echo "v0.0.0-test" ;;
  "rev-parse --short HEAD") echo "abc1234" ;;
  *) echo "unexpected git $*" >&2; exit 9 ;;
esac
SH

cat >"$FAKE_BIN/go" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "go $*" >>"$MULTICA_TEST_LOG"
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then
    out="$arg"
    break
  fi
  prev="$arg"
done
if [ -z "$out" ]; then
  echo "go build missing -o" >&2
  exit 9
fi
mkdir -p "$(dirname "$out")"
cat >"$out" <<'BIN'
#!/usr/bin/env bash
echo "multica test cli"
BIN
chmod 0755 "$out"
SH

cat >"$FAKE_BIN/brew" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "brew $*" >>"$MULTICA_TEST_LOG"
case "$*" in
  "list --formula multica") exit 0 ;;
  "uninstall multica") exit 0 ;;
  *) exit 9 ;;
esac
SH

cat >"$FAKE_BIN/install" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "install $*" >>"$MULTICA_TEST_LOG"
/usr/bin/install "$@"
SH

cat >"$FAKE_BIN/scp" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "scp $*" >>"$MULTICA_TEST_LOG"
SH

cat >"$FAKE_BIN/ssh" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "ssh $*" >>"$MULTICA_TEST_LOG"
SH

chmod 0755 "$FAKE_BIN"/*

PATH="$FAKE_BIN:$PATH" \
HOME="$HOME_DIR" \
MULTICA_REPO="$REPO" \
MULTICA_TEST_LOG="$LOG" \
MULTICA_SKIP_DEPLOY=1 \
MULTICA_SKIP_PACKAGE=1 \
MULTICA_SKIP_INSTALL=1 \
"$SCRIPT_DIR/run_release.sh" >/dev/null

grep -q 'go build .* ./cmd/multica' "$LOG"
test "$(grep -Fc 'brew list --formula multica' "$LOG")" -eq 2
test "$(grep -Fc 'brew uninstall multica' "$LOG")" -eq 2
grep -q "install -m 0755 $REPO/server/bin/multica $HOME_DIR/.local/bin/multica" "$LOG"
grep -Fq 'ssh -o RequestTTY=no my-mini zsh -lc' "$LOG"
grep -Fq "scp -o RequestTTY=no $REPO/server/bin/multica my-mini:~/.local/bin/multica.upload." "$LOG"
grep -q 'mv.*multica.upload.*multica.*version' "$LOG"

test -x "$HOME_DIR/.local/bin/multica"
