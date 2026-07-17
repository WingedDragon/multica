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
echo "git $*" >>"$MULTICA_TEST_LOG"
case "$*" in
  "rev-parse --abbrev-ref HEAD") echo "feature/selfhost-cli-update" ;;
  "status --porcelain") ;;
  "fetch upstream main --tags") ;;
  "ls-remote --exit-code --heads origin feature/selfhost-cli-update")
    if [ -n "${MULTICA_TEST_LS_REMOTE_EXIT:-}" ]; then
      exit "$MULTICA_TEST_LS_REMOTE_EXIT"
    fi
    if [ "${MULTICA_TEST_REMOTE_PUBLISHED:-1}" = "1" ]; then
      exit 0
    fi
    exit 2
    ;;
  "fetch origin feature/selfhost-cli-update") ;;
  "rev-list --count HEAD..origin/feature/selfhost-cli-update")
    echo "${MULTICA_TEST_REMOTE_AHEAD:-0}"
    ;;
  "merge-base --is-ancestor upstream/main HEAD")
    [ "${MULTICA_TEST_UPSTREAM_INTEGRATED:-0}" = "1" ]
    ;;
  "merge --no-ff upstream/main -m chore: merge upstream/main") ;;
  "rebase upstream/main") ;;
  "push --force-with-lease origin feature/selfhost-cli-update") ;;
  "push origin feature/selfhost-cli-update") ;;
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
MULTICA_UPSTREAM_SYNC_STRATEGY=rebase \
MULTICA_TEST_REMOTE_PUBLISHED=1 \
MULTICA_SKIP_DEPLOY=1 \
MULTICA_SKIP_PACKAGE=1 \
MULTICA_SKIP_INSTALL=1 \
"$SCRIPT_DIR/run_release.sh" >/dev/null

grep -Fq 'git fetch upstream main --tags' "$LOG"
grep -Fq 'git rebase upstream/main' "$LOG"
grep -Fq 'git push --force-with-lease origin feature/selfhost-cli-update' "$LOG"
grep -q 'go build .* ./cmd/multica' "$LOG"
test "$(grep -Fc 'brew list --formula multica' "$LOG")" -eq 2
test "$(grep -Fc 'brew uninstall multica' "$LOG")" -eq 2
grep -q "install -m 0755 $REPO/server/bin/multica $HOME_DIR/.local/bin/multica" "$LOG"
grep -Fq 'ssh -o RequestTTY=no my-mini zsh -lc' "$LOG"
grep -Fq "scp -o RequestTTY=no $REPO/server/bin/multica my-mini:~/.local/bin/multica.upload." "$LOG"
grep -q 'mv.*multica.upload.*multica.*version' "$LOG"

test -x "$HOME_DIR/.local/bin/multica"

run_git_only_case() {
  local name="$1"
  local strategy="$2"
  local remote_published="$3"
  local upstream_integrated="$4"
  local remote_ahead="${5:-0}"
  local case_log="$TMP/$name.log"

  : >"$case_log"
  PATH="$FAKE_BIN:$PATH" \
  HOME="$HOME_DIR" \
  MULTICA_REPO="$REPO" \
  MULTICA_TEST_LOG="$case_log" \
  MULTICA_UPSTREAM_SYNC_STRATEGY="$strategy" \
  MULTICA_TEST_REMOTE_PUBLISHED="$remote_published" \
  MULTICA_TEST_UPSTREAM_INTEGRATED="$upstream_integrated" \
  MULTICA_TEST_REMOTE_AHEAD="$remote_ahead" \
  MULTICA_SKIP_CLI_INSTALL=1 \
  MULTICA_SKIP_DEPLOY=1 \
  MULTICA_SKIP_PACKAGE=1 \
  MULTICA_SKIP_INSTALL=1 \
  "$SCRIPT_DIR/run_release.sh" >/dev/null

  echo "$case_log"
}

auto_published_log="$(run_git_only_case auto-published auto 1 0)"
grep -Fq 'git merge --no-ff upstream/main -m chore: merge upstream/main' "$auto_published_log"
grep -Fq 'git push origin feature/selfhost-cli-update' "$auto_published_log"
! grep -Fq 'git rebase upstream/main' "$auto_published_log"
! grep -Fq 'git push --force-with-lease' "$auto_published_log"

auto_unpublished_log="$(run_git_only_case auto-unpublished auto 0 0)"
grep -Fq 'git rebase upstream/main' "$auto_unpublished_log"
grep -Fq 'git push origin feature/selfhost-cli-update' "$auto_unpublished_log"
! grep -Fq 'git merge --no-ff' "$auto_unpublished_log"
! grep -Fq 'git push --force-with-lease' "$auto_unpublished_log"

already_integrated_log="$(run_git_only_case already-integrated auto 1 1)"
grep -Fq 'git push origin feature/selfhost-cli-update' "$already_integrated_log"
! grep -Fq 'git merge --no-ff' "$already_integrated_log"
! grep -Fq 'git rebase upstream/main' "$already_integrated_log"

invalid_log="$TMP/invalid.log"
: >"$invalid_log"
if PATH="$FAKE_BIN:$PATH" \
  HOME="$HOME_DIR" \
  MULTICA_REPO="$REPO" \
  MULTICA_TEST_LOG="$invalid_log" \
  MULTICA_UPSTREAM_SYNC_STRATEGY=invalid \
  MULTICA_SKIP_CLI_INSTALL=1 \
  MULTICA_SKIP_DEPLOY=1 \
  MULTICA_SKIP_PACKAGE=1 \
  MULTICA_SKIP_INSTALL=1 \
  "$SCRIPT_DIR/run_release.sh" >/dev/null 2>&1; then
  echo "invalid sync strategy should fail" >&2
  exit 1
fi

remote_ahead_log="$TMP/remote-ahead.log"
: >"$remote_ahead_log"
if PATH="$FAKE_BIN:$PATH" \
  HOME="$HOME_DIR" \
  MULTICA_REPO="$REPO" \
  MULTICA_TEST_LOG="$remote_ahead_log" \
  MULTICA_UPSTREAM_SYNC_STRATEGY=auto \
  MULTICA_TEST_REMOTE_PUBLISHED=1 \
  MULTICA_TEST_REMOTE_AHEAD=1 \
  MULTICA_SKIP_CLI_INSTALL=1 \
  MULTICA_SKIP_DEPLOY=1 \
  MULTICA_SKIP_PACKAGE=1 \
  MULTICA_SKIP_INSTALL=1 \
  "$SCRIPT_DIR/run_release.sh" >/dev/null 2>&1; then
  echo "remote-ahead branch should fail before synchronization" >&2
  exit 1
fi

! grep -Fq 'git merge --no-ff' "$remote_ahead_log"
! grep -Fq 'git rebase upstream/main' "$remote_ahead_log"
! grep -Fq 'git push ' "$remote_ahead_log"

remote_lookup_error_log="$TMP/remote-lookup-error.log"
: >"$remote_lookup_error_log"
if PATH="$FAKE_BIN:$PATH" \
  HOME="$HOME_DIR" \
  MULTICA_REPO="$REPO" \
  MULTICA_TEST_LOG="$remote_lookup_error_log" \
  MULTICA_UPSTREAM_SYNC_STRATEGY=auto \
  MULTICA_TEST_LS_REMOTE_EXIT=128 \
  MULTICA_SKIP_CLI_INSTALL=1 \
  MULTICA_SKIP_DEPLOY=1 \
  MULTICA_SKIP_PACKAGE=1 \
  MULTICA_SKIP_INSTALL=1 \
  "$SCRIPT_DIR/run_release.sh" >/dev/null 2>&1; then
  echo "remote lookup failure should abort instead of selecting rebase" >&2
  exit 1
fi

! grep -Fq 'git merge --no-ff' "$remote_lookup_error_log"
! grep -Fq 'git rebase upstream/main' "$remote_lookup_error_log"
! grep -Fq 'git push ' "$remote_lookup_error_log"

grep -Fq 'git merge --ff-only "$REMOTE_NAME/$BRANCH"' "$SCRIPT_DIR/run_release.sh"
! grep -Fq 'git reset --hard "$REMOTE_NAME/$BRANCH"' "$SCRIPT_DIR/run_release.sh"
