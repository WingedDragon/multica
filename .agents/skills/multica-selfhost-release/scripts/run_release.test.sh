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
  "status --porcelain -- apps/web/next-env.d.ts") ;;
  "restore apps/web/next-env.d.ts") ;;
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
  "rev-parse HEAD") echo "abc1234deadbeef" ;;
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
echo "multica v0.0.0-test (commit: abc1234, built: test)"
BIN
chmod 0755 "$out"
SH

cat >"$FAKE_BIN/pnpm" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "pnpm $*" >>"$MULTICA_TEST_LOG"
case "$*" in
  "--filter @multica/web typecheck") ;;
  "--filter @multica/web build")
    mkdir -p apps/web/.next
    printf 'build-id\n' >apps/web/.next/BUILD_ID
    printf '{}\n' >apps/web/.next/routes-manifest.json
    printf '{}\n' >apps/web/.next/prerender-manifest.json
    ;;
  *) echo "unexpected pnpm $*" >&2; exit 9 ;;
esac
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

cat >"$FAKE_BIN/tar" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "tar $*" >>"$MULTICA_TEST_LOG"
archive=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-czf" ]; then
    archive="$arg"
    break
  fi
  prev="$arg"
done
if [ -n "$archive" ]; then
  : >"$archive"
fi
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
grep -q 'codesign.*--force.*--sign.*multica.upload' "$LOG"
grep -q 'codesign.*--verify.*--strict.*multica' "$LOG"

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

# Deploy direct from this Mac. my-mini remains only the CLI-install target.
grep -Fq 'MY_MINI_HOST="${MULTICA_MY_MINI_HOST:-${MULTICA_REMOTE_JUMP:-my-mini}}"' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'ssh -o RequestTTY=no "$REMOTE_HOST"' "$SCRIPT_DIR/run_release.sh"
! grep -Fq 'ssh "$REMOTE_JUMP" "ssh $REMOTE_HOST' "$SCRIPT_DIR/run_release.sh"

# Web compilation runs natively on this Mac, uploads only the .next artifact,
# and leaves Linux-native dependencies and backend builds on dj.
grep -Fq 'pnpm --filter @multica/web typecheck' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'MULTICA_WEB_TYPECHECK_ALREADY_PASSED=1' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'pnpm --filter @multica/web build' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'COPYFILE_DISABLE=1 tar' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'BUILD_ID routes-manifest.json prerender-manifest.json' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'apps/web/.next.previous' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'pnpm install --frozen-lockfile' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'export PATH="$HOME/.nvm/versions/node/v24.14.0/bin:$HOME/.local/bin:$HOME/go/bin:/usr/local/go/bin:$PATH"' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'go build -ldflags' "$SCRIPT_DIR/run_release.sh"
! grep -Fq './scripts/deploy.sh' "$SCRIPT_DIR/run_release.sh"


# The remote build-pressure workaround is obsolete: Webpack runs natively on
# this Mac, while dj only installs Linux dependencies and builds Go binaries.
grep -Fq 'WEB_BUILD_MAX_OLD_SPACE_SIZE_MB="${MULTICA_WEB_BUILD_MAX_OLD_SPACE_SIZE_MB:-4096}"' "$SCRIPT_DIR/run_release.sh"
! grep -Fq 'litellm' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'sudo systemctl stop multica-frontend multica-backend' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'sudo systemctl start multica-backend multica-frontend' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'curl --noproxy "*" --retry 10 --retry-all-errors --retry-delay 1 --max-time 20 --fail --silent --show-error "$PUBLIC_URL/"' "$SCRIPT_DIR/run_release.sh"
! grep -Fq 'sudo swapoff -a' "$SCRIPT_DIR/run_release.sh"
! grep -Fq 'systemd-run' "$SCRIPT_DIR/run_release.sh"

# Every artifact and deployment must remain pinned to the HEAD selected after
# synchronization; a later fix commit requires rerunning the whole workflow.
grep -Fq 'assert_release_head' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'EXPECTED_HEAD=' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'EXPECTED_COMMIT=' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'EXPECTED_VERSION=' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'commit: $EXPECTED_COMMIT' "$SCRIPT_DIR/run_release.sh"
grep -Fq 'does not match release $EXPECTED_VERSION' "$SCRIPT_DIR/run_release.sh"

# A deploy run builds and uploads the local artifact. The fake SSH endpoint
# records the pinned remote script without changing the test host.
artifact_log="$TMP/local-artifact.log"
: >"$artifact_log"
PATH="$FAKE_BIN:$PATH" \
HOME="$HOME_DIR" \
MULTICA_REPO="$REPO" \
MULTICA_TEST_LOG="$artifact_log" \
MULTICA_UPSTREAM_SYNC_STRATEGY=merge \
MULTICA_SKIP_CLI_INSTALL=1 \
MULTICA_SKIP_PACKAGE=1 \
MULTICA_SKIP_INSTALL=1 \
"$SCRIPT_DIR/run_release.sh" >/dev/null
grep -Fq 'pnpm --filter @multica/web typecheck' "$artifact_log"
grep -Fq 'pnpm --filter @multica/web build' "$artifact_log"
grep -Fq 'tar -C' "$artifact_log"
grep -Fq 'scp -o RequestTTY=no' "$artifact_log"
grep -Fq 'dj:/tmp/multica-web-abc1234.tar.gz' "$artifact_log"
grep -Fq 'EXPECTED_HEAD=abc1234deadbeef' "$artifact_log"
grep -Fq 'REMOTE_ARTIFACT=/tmp/multica-web-abc1234.tar.gz' "$artifact_log"