#!/usr/bin/env bash
set -euo pipefail

REPO="${MULTICA_REPO:-/Users/dong/.wtc/projects/multica}"
MY_MINI_HOST="${MULTICA_MY_MINI_HOST:-${MULTICA_REMOTE_JUMP:-my-mini}}"
REMOTE_HOST="${MULTICA_REMOTE_HOST:-dj}"
REMOTE_DIR="${MULTICA_REMOTE_DIR:-/home/ubuntu/apps/multica}"
REMOTE_NAME="${MULTICA_REMOTE_NAME:-wingeddragon}"

is_https_origin() {
  local origin="$1" authority
  case "$origin" in
    https://*) authority="${origin#https://}" ;;
    *) return 1 ;;
  esac
  case "$authority" in
    ""|*/*|*\?*|*#*|*@*|*[[:space:]]*|*\\*) return 1 ;;
  esac
}

SYSTEM_HOOK_ATTESTATION="${MULTICA_GITLAB_SYSTEM_HOOKS_ATTESTED:-}"
if [ -n "$SYSTEM_HOOK_ATTESTATION" ] && ! is_https_origin "$SYSTEM_HOOK_ATTESTATION"; then
  echo "MULTICA_GITLAB_SYSTEM_HOOKS_ATTESTED must be one complete HTTPS origin without a path, user info, query, or fragment." >&2
  exit 2
fi

SKIP_DEPLOY="${MULTICA_SKIP_DEPLOY:-0}"
SKIP_PACKAGE="${MULTICA_SKIP_PACKAGE:-0}"
SKIP_INSTALL="${MULTICA_SKIP_INSTALL:-0}"
SKIP_CLI_INSTALL="${MULTICA_SKIP_CLI_INSTALL:-0}"
UPSTREAM_SYNC_STRATEGY="${MULTICA_UPSTREAM_SYNC_STRATEGY:-auto}"
EXPECTED_HEAD=""
EXPECTED_COMMIT=""
EXPECTED_VERSION=""
EXPECTED_BINARY_VERSION=""

WEB_BUILD_MAX_OLD_SPACE_SIZE_MB="${MULTICA_WEB_BUILD_MAX_OLD_SPACE_SIZE_MB:-4096}"
CLI_BIN="$REPO/server/bin/multica"
WEB_ARTIFACT=""
assert_release_head() {
  local actual
  actual="$(git rev-parse HEAD)"
  if [ -n "$EXPECTED_HEAD" ] && [ "$actual" != "$EXPECTED_HEAD" ]; then
    echo "Release HEAD changed after synchronization: expected $EXPECTED_HEAD, got $actual. Rerun the full release workflow." >&2
    exit 2
  fi
}


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

build_web_artifact() {
  echo "==> Build web artifact locally"
  local artifact_dir
  local build_status=0

  if [ -n "$(git status --porcelain -- apps/web/next-env.d.ts)" ]; then
    echo "apps/web/next-env.d.ts was dirty before the release build; refusing to overwrite user work." >&2
    exit 2
  fi

  set +e
  NODE_OPTIONS="${NODE_OPTIONS:+$NODE_OPTIONS }--max-old-space-size=${WEB_BUILD_MAX_OLD_SPACE_SIZE_MB}" \
    pnpm --filter @multica/web typecheck
  build_status=$?
  if [ "$build_status" -eq 0 ]; then
    NODE_ENV=production \
      NODE_OPTIONS="${NODE_OPTIONS:+$NODE_OPTIONS }--max-old-space-size=${WEB_BUILD_MAX_OLD_SPACE_SIZE_MB}" \
      MULTICA_WEB_TYPECHECK_ALREADY_PASSED=1 \
      pnpm --filter @multica/web build
    build_status=$?
  fi
  set -e
  git restore apps/web/next-env.d.ts >/dev/null 2>&1 || true
  if [ "$build_status" -ne 0 ]; then
    return "$build_status"
  fi

  for file in BUILD_ID routes-manifest.json prerender-manifest.json; do
    if [ ! -s "$REPO/apps/web/.next/$file" ]; then
      echo "Missing web build artifact: apps/web/.next/$file" >&2
      exit 3
    fi
  done

  artifact_dir="$(mktemp -d)"
  WEB_ARTIFACT="$artifact_dir/multica-web-$EXPECTED_COMMIT.tar.gz"
  COPYFILE_DISABLE=1 tar --no-xattrs -C "$REPO/apps/web" -czf "$WEB_ARTIFACT" .next
}

cleanup_web_artifact() {
  if [ -n "$WEB_ARTIFACT" ]; then
    rm -rf "$(dirname "$WEB_ARTIFACT")"
  fi
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
  local output
  output="$("$HOME/.local/bin/multica" version)"
  printf '%s\n' "$output"
  printf '%s\n' "$output" | grep -Fq "commit: $EXPECTED_COMMIT"
}

run_my_mini_zsh() {
  local script="$1"
  # Rebase note: use zsh -lc for my-mini so Homebrew is discoverable even from
  # a non-login ssh command; this matches prior daemon/PATH recovery work.
  ssh -o RequestTTY=no "$MY_MINI_HOST" "zsh -lc $(printf '%q' "$script")"
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
  scp -o RequestTTY=no "$CLI_BIN" "$MY_MINI_HOST:~/$remote_tmp"
  run_my_mini_zsh "chmod 0755 \"\$HOME/$remote_tmp\" && codesign --force --sign - \"\$HOME/$remote_tmp\" && mv \"\$HOME/$remote_tmp\" \"\$HOME/.local/bin/multica\" && codesign --verify --strict \"\$HOME/.local/bin/multica\" && output=\$(\"\$HOME/.local/bin/multica\" version) && printf '%s\\n' \"\$output\" && printf '%s\\n' \"\$output\" | grep -Fq 'commit: $EXPECTED_COMMIT'"
}

sync_upstream() {
  local branch="$1"
  local requested="$UPSTREAM_SYNC_STRATEGY"
  local effective="$requested"
  local remote_published=0
  local remote_ahead=0
  local remote_lookup_status=0
  local upstream_pin="${MULTICA_UPSTREAM_PIN:-}"
  local normalized_pin=""
  local resolved_pin=""

  if [ -n "$upstream_pin" ]; then
    if [[ ! "$upstream_pin" =~ ^([0-9a-fA-F]{40}|[0-9a-fA-F]{64})$ ]]; then
      echo "MULTICA_UPSTREAM_PIN must be a full 40- or 64-character commit SHA." >&2
      exit 2
    fi
    normalized_pin="$(printf '%s' "$upstream_pin" | tr '[:upper:]' '[:lower:]')"
    if ! resolved_pin="$(git rev-parse --verify "${normalized_pin}^{commit}" 2>/dev/null)"; then
      echo "MULTICA_UPSTREAM_PIN does not resolve to an existing commit: $upstream_pin." >&2
      exit 2
    fi
    if [ "$resolved_pin" != "$normalized_pin" ]; then
      echo "MULTICA_UPSTREAM_PIN must identify the commit object exactly; resolved $upstream_pin to $resolved_pin." >&2
      exit 2
    fi
    if ! git merge-base --is-ancestor "$resolved_pin" HEAD; then
      echo "MULTICA_UPSTREAM_PIN $resolved_pin is not an ancestor of HEAD; integrate and review that baseline before releasing." >&2
      exit 2
    fi
    effective="pinned"
  else
    case "$requested" in
      auto|merge|rebase) ;;
      *)
        echo "Invalid MULTICA_UPSTREAM_SYNC_STRATEGY=$requested; expected auto, merge, or rebase." >&2
        exit 2
        ;;
    esac

    git fetch upstream main --tags
  fi

  if git ls-remote --exit-code --heads origin "$branch" >/dev/null 2>&1; then
    remote_published=1
    git fetch origin "$branch"
    remote_ahead="$(git rev-list --count "HEAD..origin/$branch")"
    if [ "$remote_ahead" -gt 0 ]; then
      echo "origin/$branch contains $remote_ahead commit(s) not present locally; integrate them before release." >&2
      exit 2
    fi
  else
    remote_lookup_status=$?
    if [ "$remote_lookup_status" -ne 2 ]; then
      echo "Unable to determine whether origin/$branch exists (git ls-remote exit $remote_lookup_status); aborting before history changes." >&2
      exit 2
    fi
  fi

  if [ -z "$upstream_pin" ]; then
    if git merge-base --is-ancestor upstream/main HEAD; then
      effective="none"
    elif [ "$requested" = "auto" ]; then
      if [ "$remote_published" = "1" ]; then
        effective="merge"
      else
        effective="rebase"
      fi
    fi
    echo "==> Upstream sync: $effective (requested: $requested, published: $remote_published)"
  else
    echo "==> Upstream sync: pinned baseline $resolved_pin (upstream fetch/merge disabled, published: $remote_published)"
  fi
  case "$effective" in
    none|pinned) ;;
    merge)
      git merge --no-ff upstream/main -m "chore: merge upstream/main"
      ;;
    rebase)
      git rebase upstream/main
      ;;
  esac

  if [ "$effective" = "rebase" ] && [ "$remote_published" = "1" ]; then
    git push --force-with-lease origin "$branch"
  else
    git push origin "$branch"
  fi
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
sync_upstream "$branch"
EXPECTED_HEAD="$(git rev-parse HEAD)"
EXPECTED_COMMIT="$(git rev-parse --short HEAD)"
EXPECTED_BINARY_VERSION="$(git describe --tags --always --dirty)"
EXPECTED_VERSION="${EXPECTED_BINARY_VERSION#v}"
assert_release_head

if [ "$SKIP_CLI_INSTALL" != "1" ]; then
  assert_release_head
  build_cli
  install_local_cli
  install_my_mini_cli
fi

if [ "$SKIP_DEPLOY" != "1" ]; then
  assert_release_head
  build_web_artifact
  trap cleanup_web_artifact EXIT
fi

if [ "$SKIP_DEPLOY" != "1" ]; then
  echo "==> Remote deploy: direct ssh $REMOTE_HOST:$REMOTE_DIR"
  remote_artifact="/tmp/multica-web-$EXPECTED_COMMIT.tar.gz"
  scp -o RequestTTY=no "$WEB_ARTIFACT" "$REMOTE_HOST:$remote_artifact"
  remote_script="$(cat <<'REMOTE_SCRIPT'
set -euo pipefail
system_hook_attestation="${MULTICA_RELEASE_SYSTEM_HOOK_ATTESTATION:-}"
is_https_origin() {
  local origin="$1" authority
  case "$origin" in
    https://*) authority="${origin#https://}" ;;
    *) return 1 ;;
  esac
  case "$authority" in
    ""|*/*|*\?*|*#*|*@*|*[[:space:]]*|*\\*) return 1 ;;
  esac
}
if [ -n "$system_hook_attestation" ] && ! is_https_origin "$system_hook_attestation"; then
  echo "Invalid one-time GitLab System Hook attestation; expected an HTTPS origin without a path, user info, query, or fragment." >&2
  exit 7
fi
backend_was_active=0
frontend_was_active=0
release_complete=0
release_root=""
web_previous="apps/web/.next.previous"
bin_previous="server/bin.previous"
artifacts_installed=0
services_stopped=0
migration_up_attempted=0
migration_up_completed=0
begin_attempted=0
cutover_started=0
cutover_pending=0
cutover_finalized=0
cutover_tool=""
cutover_dir=""
cutover_since=""
cutover_generation=0
cutover_backup_generation=0
cutover_record_status=""
cutover_record_system_hook_attestation=""
cutover_record_backend=0
cutover_record_frontend=0
cutover_had_web=0
cutover_had_server=0
cutover_had_multica=0
cutover_had_migrate=0
release_smoke_url="$PUBLIC_URL"
cutover_state_file="$REMOTE_DIR/.gitlab-vcs-cutover.state"
pg_service_file=""
pg_password_file=""

json_field() {
  node -e '
    let input = "";
    process.stdin.setEncoding("utf8");
    process.stdin.on("data", chunk => input += chunk);
    process.stdin.on("end", () => {
      try {
        const value = JSON.parse(input)[process.argv[1]];
        if (Array.isArray(value)) process.stdout.write(value.join("\n"));
        else if (value !== undefined && value !== null) process.stdout.write(String(value));
      } catch {
        process.exitCode = 2;
      }
    });
  ' "$1"
}

load_workspace_env() {
  set +x
  unset DATABASE_URL PGDATABASE REDIS_URL GITLAB_BASE_URL GITLAB_TOKEN GITLAB_WEBHOOK_SECRET \
    GITLAB_PROXY_URL MULTICA_VCS_INTEGRATION_ENABLED MULTICA_VCS_SECRET_KEY MULTICA_PUBLIC_URL \
    MULTICA_RELEASE_SYSTEM_HOOK_ATTESTATION
  set -a
  . "$REMOTE_DIR/.env"
  set +a
  set +x
  if [ -z "${DATABASE_URL:-}" ]; then
    echo "Workspace .env does not define DATABASE_URL; refusing release migration." >&2
    return 1
  fi
}

cleanup_pg_connection() {
  [ -z "$pg_password_file" ] || rm -f -- "$pg_password_file"
  [ -z "$pg_service_file" ] || rm -f -- "$pg_service_file"
  pg_password_file=""
  pg_service_file=""
}

prepare_pg_connection() {
  # libpq treats a service's dbname as a literal; split URI fields into the service and keep its password in a private passfile.
  [ -n "$pg_service_file" ] && return 0
  local previous_umask
  previous_umask="$(umask)"
  umask 077
  pg_service_file="$(mktemp "${TMPDIR:-/tmp}/multica-pg-service.XXXXXX")"
  pg_password_file="$(mktemp "${TMPDIR:-/tmp}/multica-pg-password.XXXXXX")"
  chmod 0600 "$pg_service_file" "$pg_password_file"
  umask "$previous_umask"

  if ! node - "$pg_service_file" "$pg_password_file" <<'PGURL'
const fs = require("node:fs");
const servicePath = process.argv[2];
const passwordPath = process.argv[3];
const values = new Map();

try {
  const uri = process.env.DATABASE_URL || "";
  const prefix = /^postgres(?:ql)?:\/\//i;
  if (!prefix.test(uri)) throw new Error();
  const rest = uri.replace(prefix, "");
  const queryIndex = rest.indexOf("?");
  const addressAndPath = queryIndex < 0 ? rest : rest.slice(0, queryIndex);
  const rawQuery = queryIndex < 0 ? "" : rest.slice(queryIndex + 1);
  const slashIndex = addressAndPath.indexOf("/");
  let authority = slashIndex < 0 ? addressAndPath : addressAndPath.slice(0, slashIndex);
  const rawDatabase = slashIndex < 0 ? "" : addressAndPath.slice(slashIndex + 1);
  const decode = (value) => {
    const decoded = decodeURIComponent(value);
    if (decoded.includes("\0")) throw new Error();
    return decoded;
  };

  const userInfoIndex = authority.indexOf("@");
  if (userInfoIndex >= 0) {
    const userInfo = authority.slice(0, userInfoIndex);
    authority = authority.slice(userInfoIndex + 1);
    const colonIndex = userInfo.indexOf(":");
    const user = colonIndex < 0 ? userInfo : userInfo.slice(0, colonIndex);
    const password = colonIndex < 0 ? null : userInfo.slice(colonIndex + 1);
    if (user !== "") values.set("user", decode(user));
    if (password !== null && password !== "") values.set("password", decode(password));
  }

  const hosts = [];
  const ports = [];
  let hasPort = false;
  let position = 0;
  while (position < authority.length) {
    let rawHost;
    let rawPort = null;
    if (authority[position] === "[") {
      const closingBracket = authority.indexOf("]", position + 1);
      if (closingBracket < 0 || closingBracket === position + 1) throw new Error();
      rawHost = authority.slice(position + 1, closingBracket);
      position = closingBracket + 1;
      if (position < authority.length && authority[position] === ":") {
        const start = ++position;
        while (position < authority.length && authority[position] !== ",") position++;
        rawPort = authority.slice(start, position);
      } else if (position < authority.length && authority[position] !== ",") {
        throw new Error();
      }
    } else {
      const start = position;
      while (position < authority.length && authority[position] !== ":" && authority[position] !== ",") position++;
      rawHost = authority.slice(start, position);
      if (position < authority.length && authority[position] === ":") {
        const portStart = ++position;
        while (position < authority.length && authority[position] !== ",") position++;
        rawPort = authority.slice(portStart, position);
      }
    }
    hosts.push(decode(rawHost));
    ports.push(rawPort === null ? "" : decode(rawPort));
    if (rawPort !== null) hasPort = true;
    if (position < authority.length) {
      if (authority[position] !== "," || position + 1 === authority.length) throw new Error();
      position++;
    }
  }
  if (hosts.join(",") !== "") values.set("host", hosts.join(","));
  if (hasPort) values.set("port", ports.join(","));
  if (rawDatabase !== "") values.set("dbname", decode(rawDatabase));

  if (rawQuery !== "") {
    for (const parameter of rawQuery.split("&")) {
      const equalsIndex = parameter.indexOf("=");
      if (equalsIndex < 1) throw new Error();
      let key = decode(parameter.slice(0, equalsIndex));
      let value = decode(parameter.slice(equalsIndex + 1));
      if (key === "ssl" && (value === "true" || value === "false")) {
        key = "sslmode";
        value = value === "true" ? "require" : "disable";
      }
      if (key === "service") throw new Error();
      values.set(key, value);
    }
  }

  const password = values.get("password");
  values.delete("password");
  if (password) {
    if (/[\r\n]/.test(password)) throw new Error();
    const escapedPassword = password.replace(/\\/g, "\\\\").replace(/:/g, "\\:");
    fs.writeFileSync(passwordPath, `*:*:*:*:${escapedPassword}\n`, { mode: 0o600 });
    values.set("passfile", passwordPath);
  }

  const lines = [];
  for (const [key, value] of values) {
    if (!/^[a-z_][a-z0-9_]*$/.test(key) || /[\0\r\n]/.test(value) || /^\s|\s$/.test(value)) throw new Error();
    const line = `${key}=${value}`;
    if (Buffer.byteLength(`${line}\n`, "utf8") >= 1023) throw new Error();
    lines.push(line);
  }
  fs.writeFileSync(servicePath, `[multica_cutover]\n${lines.join("\n")}\n`, { mode: 0o600 });
} catch {
  process.exitCode = 1;
}
PGURL
  then
    cleanup_pg_connection
    echo "Workspace DATABASE_URL is not a supported PostgreSQL connection URI; refusing database access." >&2
    return 1
  fi
  chmod 0600 "$pg_service_file" "$pg_password_file"
}

run_workspace_pg() (
  unset DATABASE_URL REDIS_URL GITLAB_BASE_URL GITLAB_TOKEN GITLAB_WEBHOOK_SECRET \
    GITLAB_PROXY_URL MULTICA_VCS_INTEGRATION_ENABLED MULTICA_VCS_SECRET_KEY MULTICA_PUBLIC_URL \
    MULTICA_RELEASE_SYSTEM_HOOK_ATTESTATION
  for variable in ${!PG@}; do unset "$variable"; done
  PGSERVICEFILE="$pg_service_file" PGSERVICE=multica_cutover "$@"
)

read_cutover_record() {
  cutover_record_head=""
  cutover_record_status=""
  cutover_record_system_hook_attestation=""
  cutover_dir=""
  cutover_since=""
  cutover_generation=0
  cutover_record_backend=0
  cutover_record_frontend=0
  [ -f "$cutover_state_file" ] || return 1
  while IFS='=' read -r key value; do
    case "$key" in
      head) cutover_record_head="$value" ;;
      status) cutover_record_status="$value" ;;
      system_hook_attestation) cutover_record_system_hook_attestation="$value" ;;
      backup_dir) cutover_dir="$value" ;;
      since) cutover_since="$value" ;;
      generation) cutover_generation="$value" ;;
      backend_was_active) cutover_record_backend="$value" ;;
      frontend_was_active) cutover_record_frontend="$value" ;;
    esac
  done <"$cutover_state_file"
  case "$cutover_dir" in .gitlab-vcs-cutover-*) ;; *) echo "Invalid GitLab cutover recovery directory; refusing to proceed." >&2; return 1 ;; esac
  case "$cutover_generation" in ''|*[!0-9]*) echo "Invalid GitLab cutover generation; refusing to proceed." >&2; return 1 ;; esac
  if [ -n "$cutover_record_system_hook_attestation" ] && ! is_https_origin "$cutover_record_system_hook_attestation"; then
    echo "Invalid GitLab System Hook attestation in cutover recovery record; refusing to proceed." >&2
    return 1
  fi
}

write_cutover_record() {
  local status="$1"
  local temporary="$cutover_state_file.tmp.$$"
  umask 077
  {
    printf 'head=%s\n' "$EXPECTED_HEAD"
    printf 'status=%s\n' "$status"
    printf 'backup_dir=%s\n' "$cutover_dir"
    printf 'since=%s\n' "$cutover_since"
    printf 'generation=%s\n' "$cutover_generation"
    printf 'system_hook_attestation=%s\n' "$system_hook_attestation"
    printf 'backend_was_active=%s\n' "$backend_was_active"
    printf 'frontend_was_active=%s\n' "$frontend_was_active"
  } >"$temporary"
  chmod 0600 "$temporary"
  mv -f "$temporary" "$cutover_state_file"
}

read_cli_state() {
  local json="$1"
  cutover_phase="$(json_field phase <<<"$json")"
  cutover_maintenance="$(json_field maintenance_mode <<<"$json")"
  cutover_generation="$(json_field generation <<<"$json")"
  [ -n "$cutover_phase" ] && [ -n "$cutover_maintenance" ] && [ -n "$cutover_generation" ]
}

capture_recovery_deliveries() {
  local json="$1"
  local ids delivery_file temporary
  ids="$(json_field recovery_delivery_ids <<<"$json" 2>/dev/null || true)"
  [ -n "$ids" ] || return 0
  [ -n "$cutover_dir" ] || return 0
  delivery_file="$REMOTE_DIR/$cutover_dir/delivery-ids.txt"
  temporary="$delivery_file.tmp.$$"
  umask 077
  {
    [ ! -f "$delivery_file" ] || cat "$delivery_file"
    printf '%s\n' "$ids"
  } | sort -u >"$temporary"
  chmod 0600 "$temporary"
  mv -f "$temporary" "$delivery_file"
}

report_recovery_deliveries() {
  local json="$1"
  local ids current_ids delivery_file
  current_ids="$(json_field recovery_delivery_ids <<<"$json" 2>/dev/null || true)"
  capture_recovery_deliveries "$json"
  ids="$current_ids"
  if [ -n "$cutover_dir" ]; then
    delivery_file="$REMOTE_DIR/$cutover_dir/delivery-ids.txt"
    if [ -s "$delivery_file" ]; then ids="$(cat "$delivery_file")"; fi
  fi
  if [ -n "$ids" ]; then
    echo "GitLab maintenance delivery IDs that received HTTP 503:" >&2
    while IFS= read -r id; do
      [ -z "$id" ] || printf '  %s\n' "$id" >&2
    done <<<"$ids"
    echo "After Finalize or old-service restoration, reconcile the listed IDs with delivery evidence available on this GitLab version and redeliver failures; explicitly acknowledge any unrecoverable ID. GitLab 13.12.15 has no delivery-history API, so this list cannot enumerate attempts that were never persisted here. If no version-supported source evidence is available, report those attempts as unverified and do not claim complete event recovery." >&2
  else
    echo "No maintenance delivery IDs were persisted. GitLab 13.12.15 has no delivery-history API, so unpersisted attempts cannot be enumerated here. Use any version-supported source evidence, redeliver failures after Finalize or old-service restoration, and report any unverifiable attempts as unverified; do not claim complete event recovery." >&2
  fi
}

save_cutover_artifacts() {
  local metadata="$cutover_dir/artifacts.state"
  [ -f "$metadata" ] && return 0
  mkdir -m 0700 -p "$cutover_dir/artifacts"
  cutover_had_web=0
  cutover_had_server=0
  cutover_had_multica=0
  cutover_had_migrate=0
  if [ -d apps/web/.next ]; then
    cp -a apps/web/.next "$cutover_dir/artifacts/web.previous"
    cutover_had_web=1
  fi
  for binary in server multica migrate; do
    if [ -f "server/bin/$binary" ]; then
      cp -p "server/bin/$binary" "$cutover_dir/artifacts/$binary.previous"
      case "$binary" in
        server) cutover_had_server=1 ;;
        multica) cutover_had_multica=1 ;;
        migrate) cutover_had_migrate=1 ;;
      esac
    fi
  done
  {
    printf 'web=%s\n' "$cutover_had_web"
    printf 'server=%s\n' "$cutover_had_server"
    printf 'multica=%s\n' "$cutover_had_multica"
    printf 'migrate=%s\n' "$cutover_had_migrate"
  } >"$metadata"
  chmod 0600 "$metadata"
}

restore_cutover_artifacts() {
  local key value had
  [ -f "$cutover_dir/artifacts.state" ] || {
    echo "Cutover artifact snapshot is missing; refusing to start the old release." >&2
    return 1
  }
  cutover_had_web=0
  cutover_had_server=0
  cutover_had_multica=0
  cutover_had_migrate=0
  while IFS='=' read -r key value; do
    case "$key" in
      web) cutover_had_web="$value" ;;
      server) cutover_had_server="$value" ;;
      multica) cutover_had_multica="$value" ;;
      migrate) cutover_had_migrate="$value" ;;
    esac
  done <"$cutover_dir/artifacts.state"
  rm -rf apps/web/.next
  if [ "$cutover_had_web" = "1" ]; then
    cp -a "$cutover_dir/artifacts/web.previous" apps/web/.next
  fi
  for binary in server multica migrate; do
    case "$binary" in
      server) had="$cutover_had_server" ;;
      multica) had="$cutover_had_multica" ;;
      migrate) had="$cutover_had_migrate" ;;
    esac
    if [ "$had" = "1" ]; then
      cp -p "$cutover_dir/artifacts/$binary.previous" "server/bin/$binary"
    else
      rm -f "server/bin/$binary"
    fi
  done
}

restore_normal_artifacts() {
  [ "$artifacts_installed" = "1" ] || return 0
  rm -rf apps/web/.next
  if [ -d "$web_previous" ]; then mv "$web_previous" apps/web/.next; fi
  if [ -d "$bin_previous" ]; then
    for binary in server multica migrate; do
      if [ -f "$bin_previous/$binary" ]; then
        cp -p "$bin_previous/$binary" "server/bin/$binary"
      else
        rm -f "server/bin/$binary"
      fi
    done
  fi
}

start_pre_release_services() {
  if [ "$backend_was_active" = "1" ]; then
    sudo systemctl start multica-backend || return 1
    systemctl is-active --quiet multica-backend || return 1
  fi
  if [ "$frontend_was_active" = "1" ]; then
    sudo systemctl start multica-frontend || return 1
    systemctl is-active --quiet multica-frontend || return 1
  fi
}

restore_cutover_after_failure() {
  local original_status="$1"
  local restore_output restore_status state_json state_status
  set +e
  if [ -z "$cutover_tool" ] || [ ! -x "$cutover_tool" ]; then
    report_recovery_deliveries ""
    echo "GitLab cutover restore tool is unavailable. DO NOT start the old version; keep the deployment in maintenance and restore the pinned tool." >&2
    return 1
  fi
  if ! sudo systemctl stop multica-frontend multica-backend >/dev/null 2>&1; then
    state_json="$("$cutover_tool" status 2>&1 || true)"
    report_recovery_deliveries "$state_json"
    echo "Could not stop all services before GitLab restore. The Hook/database restore was not attempted; keep maintenance closed and stop every writer before retrying." >&2
    return 1
  fi
  if systemctl is-active --quiet multica-backend || systemctl is-active --quiet multica-frontend; then
    state_json="$("$cutover_tool" status 2>&1 || true)"
    report_recovery_deliveries "$state_json"
    echo "A service remains active before GitLab restore. The Hook/database restore was not attempted; keep maintenance closed and stop every writer before retrying." >&2
    return 1
  fi
  if [ -z "$cutover_dir" ] || [ ! -f "$cutover_dir/artifacts.state" ]; then
    state_json="$("$cutover_tool" status 2>&1 || true)"
    report_recovery_deliveries "$state_json"
    echo "The durable pre-release artifact snapshot is unavailable. The Hook/database restore was not attempted; keep services stopped and recover the pinned pre-release artifacts before retrying." >&2
    return 1
  fi
  restore_output="$("$cutover_tool" restore 2>&1)"
  restore_status=$?
  if [ -n "$restore_output" ]; then printf '%s\n' "$restore_output" >&2; fi
  state_json="$("$cutover_tool" status 2>&1)"
  state_status=$?
  if [ "$state_status" -ne 0 ] || ! read_cli_state "$state_json"; then
    report_recovery_deliveries "$state_json"
    echo "GitLab Hook/database restore could not be verified. DO NOT start the old version or allow normal writes; keep the deployment in maintenance and resolve the reported restore state." >&2
    return 1
  fi
  if [ "$restore_status" -ne 0 ] || [ "$cutover_phase" != "restored" ] || [ "$cutover_maintenance" != "true" ]; then
    report_recovery_deliveries "$state_json"
    echo "GitLab Hook/database restore is incomplete (phase=$cutover_phase). DO NOT start the old version or allow normal writes; keep the deployment in maintenance." >&2
    return 1
  fi
  if ! activate_restored_pre_release "$state_json"; then
    return 1
  fi
  return "$original_status"
}

activate_restored_pre_release() {
  local state_json="$1"
  if [ -z "$cutover_dir" ] || [ ! -f "$cutover_dir/artifacts.state" ]; then
    report_recovery_deliveries "$state_json"
    echo "The durable pre-release artifact snapshot is unavailable. Keep services stopped and recover the pinned pre-release artifacts before restarting." >&2
    return 1
  fi
  if ! restore_cutover_artifacts; then
    report_recovery_deliveries "$state_json"
    echo "GitLab data and Hooks were restored, but application artifacts were not. Keep services stopped; restore the pinned pre-release binaries before restarting." >&2
    return 1
  fi
  if ! start_pre_release_services; then
    sudo systemctl stop multica-frontend multica-backend >/dev/null 2>&1 || true
    report_recovery_deliveries "$state_json"
    if systemctl is-active --quiet multica-backend || systemctl is-active --quiet multica-frontend; then
      echo "GitLab Hooks and database were restored, but a partially started old service could not be stopped; keep writes closed and resolve service recovery manually." >&2
    else
      echo "GitLab Hooks and database were restored, but the old service did not become healthy; both services were returned to stopped state." >&2
    fi
    return 1
  fi
  cutover_generation="$(json_field generation <<<"$state_json")"
  if ! write_cutover_record restored; then
    report_recovery_deliveries "$state_json"
    echo "GitLab data, Hooks, and old artifacts were restored, but the durable recovery record could not be updated. Resolve the release state manually." >&2
    return 1
  fi
  report_recovery_deliveries "$state_json"
  echo "GitLab Hook and database restore completed; the pre-release service state was restored. The GitLab 503 deliveries above still require redelivery or explicit operator acknowledgment; this release is not complete." >&2
}

restore_release_on_exit() {
  local status="$?"
  local state_json state_status state_phase state_maintenance state_generation
  trap - EXIT
  cleanup_pg_connection
  if [ "$status" -eq 0 ] || [ "$release_complete" = "1" ]; then exit "$status"; fi
  if [ "$cutover_finalized" = "1" ]; then
    echo "GitLab cutover is finalized and normal writes may be open. Do not restore the old database or binaries; keep the pinned new release and investigate forward." >&2
    exit "$status"
  fi
  if [ "$cutover_pending" = "1" ]; then
    echo "GitLab cutover remains gated in maintenance and is resumable from the pinned HEAD. Do not start an old writer; complete Hook setup/delivery verification or run the full restore." >&2
    exit "$status"
  fi
  if [ "$services_stopped" = "1" ] && [ "$migration_up_attempted" = "1" ] && [ "$migration_up_completed" != "1" ] && [ "$cutover_started" != "1" ]; then
    echo "Application migrations did not complete after the read-only GitLab preflight. Do not query cutover status until cmd/migrate up succeeds; keep services stopped and retry the exact pinned HEAD after resolving the migration failure." >&2
    if [ -n "$cutover_dir" ]; then echo "Pinned pre-cutover database and artifact snapshot: $cutover_dir; restore it before restarting old services if the migration cannot be resumed safely." >&2; fi
    exit "$status"
  fi
  if [ "$services_stopped" = "1" ] && [ "$migration_up_attempted" = "1" ] && [ "$migration_up_completed" = "1" ] && [ "$cutover_started" != "1" ]; then
    if load_workspace_env && [ -n "$release_root" ] && [ -x "$release_root/bin/gitlab-vcs-migrate-$EXPECTED_HEAD" ]; then
      cutover_tool="$release_root/bin/gitlab-vcs-migrate-$EXPECTED_HEAD"
      state_json="$("$cutover_tool" status 2>/dev/null)"
      state_status=$?
      if [ "$state_status" -eq 0 ] && read_cli_state "$state_json"; then
        state_phase="$cutover_phase"
        state_maintenance="$cutover_maintenance"
        state_generation="$cutover_generation"
        if [ "$state_maintenance" = "true" ] && [ "$state_phase" != "complete" ]; then
          if [ "$state_phase" = "maintenance" ] && [ "$state_generation" = "0" ] && [ "$begin_attempted" != "1" ]; then
            cutover_started=0
          else
            cutover_started=1
          fi
        elif [ "$state_maintenance" != "false" ] || [ "$state_phase" != "complete" ]; then
          echo "Migration state is not safely readable as a complete no-op. Leave services stopped and inspect the migration state before rollback." >&2
          exit "$status"
        fi
      else
        echo "Could not verify GitLab migration state after an interrupted schema migration. Leave services stopped; do not start an old writer until state is confirmed." >&2
        exit "$status"
      fi
    else
      echo "Could not load the workspace environment or pinned migration tool. Leave services stopped and verify the database state before restarting." >&2
      exit "$status"
    fi
  fi
  if [ "$cutover_started" = "1" ]; then
    restore_cutover_after_failure "$status"
    exit "$status"
  fi
  if [ "$services_stopped" != "1" ]; then exit "$status"; fi
  if [ "$artifacts_installed" = "1" ]; then
    if ! sudo systemctl stop multica-frontend multica-backend >/dev/null 2>&1; then
      echo "Could not stop new services for artifact rollback. Keep writers closed and restore the deployment manually." >&2
      exit "$status"
    fi
    if systemctl is-active --quiet multica-backend || systemctl is-active --quiet multica-frontend; then
      echo "A service is still active after stop; refusing to replace its binaries." >&2
      exit "$status"
    fi
    restore_normal_artifacts || {
      echo "Release artifacts could not be restored. Keep services stopped and restore the saved previous release before restarting." >&2
      exit "$status"
    }
  fi
  if ! start_pre_release_services; then
    echo "The previous service state could not be restored; inspect systemd before reopening writes." >&2
  fi
  exit "$status"
}
trap restore_release_on_exit EXIT
export PATH="$HOME/.nvm/versions/node/v24.14.0/bin:$HOME/.local/bin:$HOME/go/bin:/usr/local/go/bin:$PATH"

cd "$REMOTE_DIR"
if [ -f "$cutover_state_file" ]; then
  read_cutover_record || exit 7
  if [ "$cutover_record_status" = "preparing" ] && [ "$cutover_record_head" != "$EXPECTED_HEAD" ]; then
    echo "An interrupted pre-migration GitLab release is pinned to $cutover_record_head. Rerun that exact HEAD before deploying $EXPECTED_HEAD; do not mix release commits." >&2
    exit 7
  fi
  if [ "$cutover_record_status" = "active" ] && [ "$cutover_record_head" != "$EXPECTED_HEAD" ]; then
    if ! load_workspace_env; then
      echo "An unfinished GitLab cutover is pinned to $cutover_record_head; workspace environment is unavailable." >&2
      exit 7
    fi
    prior_tool="$REMOTE_DIR/$cutover_dir/gitlab-vcs-migrate-$cutover_record_head"
    if [ ! -x "$prior_tool" ]; then
      echo "An unfinished GitLab cutover is pinned to $cutover_record_head and its pinned recovery tool is missing. Refusing to mix release commits." >&2
      exit 7
    fi
    prior_state="$("$prior_tool" status 2>&1)" || {
      echo "Could not verify the unfinished GitLab cutover pinned to $cutover_record_head; refusing to change the remote checkout." >&2
      exit 7
    }
    prior_phase="$(json_field phase <<<"$prior_state")"
    prior_maintenance="$(json_field maintenance_mode <<<"$prior_state")"
    case "$prior_phase:$prior_maintenance" in
      complete:false) ;;
      restored:true)
        echo "The pinned cutover has restored Hooks/database but has not durably completed old-service recovery. Rerun its exact HEAD $cutover_record_head before deploying $EXPECTED_HEAD." >&2
        exit 7
        ;;
      *)
        echo "GitLab cutover phase $prior_phase (maintenance=$prior_maintenance) is not a finalized, un-gated boundary. Resume or fully restore exact HEAD $cutover_record_head before deploying $EXPECTED_HEAD." >&2
        exit 7
        ;;
    esac
  fi
fi

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
  echo "Remote checkout cannot fast-forward; refusing to reset or discard commits automatically." >&2
  exit 5
fi
if [ "$(git rev-parse HEAD)" != "$EXPECTED_HEAD" ]; then
  echo "Remote checkout does not match release HEAD $EXPECTED_HEAD" >&2
  exit 6
fi
if [ -f "$cutover_state_file" ]; then
  read_cutover_record || exit 7
  if { [ "$cutover_record_status" = "active" ] || [ "$cutover_record_status" = "preparing" ]; } && [ "$cutover_record_head" = "$EXPECTED_HEAD" ]; then
    backend_was_active="$cutover_record_backend"
    frontend_was_active="$cutover_record_frontend"
    cutover_tool="$REMOTE_DIR/$cutover_dir/gitlab-vcs-migrate-$EXPECTED_HEAD"
    if [ ! -x "$cutover_tool" ]; then
      echo "An interrupted GitLab cutover is pinned to this HEAD but its recovery tool is missing. Refusing to install packages or mix release commits." >&2
      exit 7
    fi
    if ! load_workspace_env; then
      echo "An interrupted GitLab cutover is pinned to this HEAD but the workspace environment is unavailable." >&2
      exit 7
    fi
    if [ "$cutover_record_status" = "active" ]; then
      recovery_state="$("$cutover_tool" status 2>&1)" || {
        echo "Could not read the pinned GitLab cutover status before release preparation." >&2
        exit 7
      }
      read_cli_state "$recovery_state" || {
        echo "Pinned GitLab cutover status is invalid; refusing release preparation." >&2
        exit 7
      }
      case "$cutover_phase" in
        restoring|restore_blocked)
          if [ "$cutover_maintenance" != "true" ]; then
            echo "Interrupted GitLab restore is not maintenance-gated; refusing to start writers or alter the database." >&2
            exit 7
          fi
          cutover_pending=1
          cutover_started=1
          services_stopped=1
          echo "Resuming only the interrupted Hook/database restore for the pinned HEAD; no migration, package install, or new release build will run."
          set +e
          restore_cutover_after_failure 7
          recovery_status=$?
          set -e
          if [ "$recovery_status" -eq 7 ]; then
            trap - EXIT
            echo "The previous release has been restored. This recovery-only invocation did not deploy; redeliver listed GitLab 503 deliveries, then rerun the release if still needed." >&2
          fi
          exit 7
          ;;
        restored)
          if [ "$cutover_maintenance" != "true" ]; then
            echo "GitLab restore is not confirmed as maintenance-gated; refusing to start the old service." >&2
            exit 7
          fi
          cutover_pending=1
          services_stopped=1
          if ! sudo systemctl stop multica-frontend multica-backend >/dev/null 2>&1; then
            echo "Could not stop all services before resuming the restored pre-release app. Keep maintenance closed and resolve service state manually." >&2
            exit 7
          fi
          if systemctl is-active --quiet multica-backend || systemctl is-active --quiet multica-frontend; then
            echo "A service remains active; refusing to replace artifacts or resume the old app." >&2
            exit 7
          fi
          if ! activate_restored_pre_release "$recovery_state"; then exit 7; fi
          trap - EXIT
          echo "The previous release has been restored. This recovery-only invocation did not deploy; redeliver listed GitLab 503 deliveries, then rerun the release if still needed." >&2
          exit 7
          ;;
      esac
    fi
  else
    backend_was_active=0
    frontend_was_active=0
    systemctl is-active --quiet multica-backend && backend_was_active=1
    systemctl is-active --quiet multica-frontend && frontend_was_active=1
  fi
else
  backend_was_active=0
  frontend_was_active=0
  systemctl is-active --quiet multica-backend && backend_was_active=1
  systemctl is-active --quiet multica-frontend && frontend_was_active=1
fi


pnpm install --frozen-lockfile

release_root="$(mktemp -d "$REMOTE_DIR/.release-$EXPECTED_COMMIT.XXXXXX")"
tar -xzf "$REMOTE_ARTIFACT" -C "$release_root"
for file in BUILD_ID routes-manifest.json prerender-manifest.json; do
  test -s "$release_root/.next/$file"
done

mkdir -p "$release_root/bin"
VERSION="$EXPECTED_BINARY_VERSION"
COMMIT="$(git rev-parse --short HEAD)"
DATE="$(date -u "+%Y-%m-%dT%H:%M:%SZ")"
(
  cd server
  go build -ldflags "-X main.version=$VERSION -X main.commit=$COMMIT" -o "$release_root/bin/server" ./cmd/server
  go build -ldflags "-X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" -o "$release_root/bin/multica" ./cmd/multica
  go build -o "$release_root/bin/migrate" ./cmd/migrate
  go build -o "$release_root/bin/gitlab-vcs-migrate-$EXPECTED_HEAD" ./cmd/gitlab-vcs-migrate
)

if [ -f "$cutover_state_file" ]; then
  read_cutover_record || exit 7
  if { [ "$cutover_record_status" = "active" ] || [ "$cutover_record_status" = "preparing" ]; } && [ "$cutover_record_head" = "$EXPECTED_HEAD" ]; then
    backend_was_active="$cutover_record_backend"
    frontend_was_active="$cutover_record_frontend"
    cutover_tool="$REMOTE_DIR/$cutover_dir/gitlab-vcs-migrate-$EXPECTED_HEAD"
    if [ ! -x "$cutover_tool" ]; then
      echo "The pinned GitLab cutover tool is missing. Refusing to resume or mix commits; keep services stopped and restore the exact pinned tool." >&2
      exit 7
    fi
  else
    backend_was_active=0
    frontend_was_active=0
    systemctl is-active --quiet multica-backend && backend_was_active=1
    systemctl is-active --quiet multica-frontend && frontend_was_active=1
  fi
else
  backend_was_active=0
  frontend_was_active=0
  systemctl is-active --quiet multica-backend && backend_was_active=1
  systemctl is-active --quiet multica-frontend && frontend_was_active=1
fi

load_workspace_env
if [ -n "${REDIS_URL:-}" ]; then
  legacy_data_sql="$(cat <<'SQL'
SELECT (to_regclass(format('%I.%I', current_schema(), 'gitlab_connection')) IS NOT NULL)::text AS has_gitlab_connection \gset
\if :has_gitlab_connection
SELECT EXISTS (SELECT 1 FROM gitlab_connection)::text AS has_gitlab_connection_rows \gset
\else
\set has_gitlab_connection_rows false
\endif
SELECT (to_regclass(format('%I.%I', current_schema(), 'gitlab_project_binding')) IS NOT NULL)::text AS has_gitlab_project_binding \gset
\if :has_gitlab_project_binding
SELECT EXISTS (SELECT 1 FROM gitlab_project_binding)::text AS has_gitlab_project_binding_rows \gset
\else
\set has_gitlab_project_binding_rows false
\endif
SELECT (to_regclass(format('%I.%I', current_schema(), 'gitlab_merge_request')) IS NOT NULL)::text AS has_gitlab_merge_request \gset
\if :has_gitlab_merge_request
SELECT EXISTS (SELECT 1 FROM gitlab_merge_request)::text AS has_gitlab_merge_request_rows \gset
\else
\set has_gitlab_merge_request_rows false
\endif
SELECT (to_regclass(format('%I.%I', current_schema(), 'issue_gitlab_merge_request')) IS NOT NULL)::text AS has_issue_gitlab_merge_request \gset
\if :has_issue_gitlab_merge_request
SELECT EXISTS (SELECT 1 FROM issue_gitlab_merge_request)::text AS has_issue_gitlab_merge_request_rows \gset
\else
\set has_issue_gitlab_merge_request_rows false
\endif
SELECT (to_regclass(format('%I.%I', current_schema(), 'gitlab_mr_pipeline')) IS NOT NULL)::text AS has_gitlab_mr_pipeline \gset
\if :has_gitlab_mr_pipeline
SELECT EXISTS (SELECT 1 FROM gitlab_mr_pipeline)::text AS has_gitlab_mr_pipeline_rows \gset
\else
\set has_gitlab_mr_pipeline_rows false
\endif
SELECT (to_regclass(format('%I.%I', current_schema(), 'chat_wakeup')) IS NOT NULL)::text AS has_chat_wakeup \gset
\if :has_chat_wakeup
SELECT EXISTS (
  SELECT 1 FROM chat_wakeup
  WHERE condition->>'provider'='gitlab'
    AND condition->'gitlab'->>'source'='gitlab_connection'
)::text AS has_gitlab_chat_wakeup \gset
\else
\set has_gitlab_chat_wakeup false
\endif
SELECT (
  (:has_gitlab_connection)::boolean::integer
  + (:has_gitlab_project_binding)::boolean::integer
  + (:has_gitlab_merge_request)::boolean::integer
  + (:has_issue_gitlab_merge_request)::boolean::integer
  + (:has_gitlab_mr_pipeline)::boolean::integer
) AS legacy_table_count \gset
SELECT CASE
  WHEN :legacy_table_count = 0 THEN 'f'
  WHEN :legacy_table_count <> 5 THEN 'partial'
  WHEN :has_gitlab_connection_rows
    OR :has_gitlab_project_binding_rows
    OR :has_gitlab_merge_request_rows
    OR :has_issue_gitlab_merge_request_rows
    OR :has_gitlab_mr_pipeline_rows
    OR :has_gitlab_chat_wakeup
  THEN 't'
  ELSE 'f'
END;
SQL
)"
  prepare_pg_connection || exit 7
  if ! legacy_data="$(run_workspace_pg psql -XAtq -v ON_ERROR_STOP=1 <<<"$legacy_data_sql" 2>/dev/null)"; then
    echo "GitLab cutover is blocked before stopping services: the read-only legacy-schema check could not determine whether old tables are absent, empty, partial, or contain data. Resolve the database/schema error and retry; no service, migration, or Hook changes were made." >&2
    exit 7
  fi
  case "$legacy_data" in
    t|true|1)
      echo "Redis-backed legacy GitLab cutover is blocked before stopping services: REDIS_URL enables WeCom RelayOutbound's bounded per-process queue, and this binary has no per-replica ingress-quiesce/drain acknowledgement. systemctl stop and checking Redis queues do not prove the in-process queue is drained. There is no supported manual command to unblock this release; first deploy a compatible relay with a verifiable per-replica drain acknowledgement and update release validation. No migration or Hook changes were made." >&2
      exit 7
      ;;
    partial)
      echo "GitLab cutover is blocked before stopping services: the legacy GitLab tables are only partially present. Resolve the schema inconsistency and rerun the read-only preflight; no service, migration, or Hook changes were made." >&2
      exit 7
      ;;
    f|false|0) ;;
    *)
      echo "GitLab cutover is blocked before stopping services: legacy-data preflight returned an unknown result while REDIS_URL is configured." >&2
      exit 7
      ;;
  esac
fi

if [ -f "$cutover_state_file" ]; then
  read_cutover_record || exit 7
  if [ -n "$cutover_record_system_hook_attestation" ]; then
    if [ -n "$system_hook_attestation" ] && [ "$system_hook_attestation" != "$cutover_record_system_hook_attestation" ]; then
      echo "The supplied GitLab System Hook attestation conflicts with the pinned cutover record; refusing to proceed." >&2
      exit 7
    fi
    system_hook_attestation="$cutover_record_system_hook_attestation"
  fi
  if { [ "$cutover_record_status" = "active" ] || [ "$cutover_record_status" = "preparing" ]; } && [ "$cutover_record_head" = "$EXPECTED_HEAD" ]; then
    backend_was_active="$cutover_record_backend"
    frontend_was_active="$cutover_record_frontend"
    cutover_tool="$REMOTE_DIR/$cutover_dir/gitlab-vcs-migrate-$EXPECTED_HEAD"
    if [ ! -x "$cutover_tool" ]; then
      echo "The pinned GitLab cutover tool is missing. Refusing to resume or mix commits; keep services stopped and restore the exact pinned tool." >&2
      exit 7
    fi
  else
    backend_was_active=0
    frontend_was_active=0
    systemctl is-active --quiet multica-backend && backend_was_active=1
    systemctl is-active --quiet multica-frontend && frontend_was_active=1
  fi
else
  backend_was_active=0
  frontend_was_active=0
  systemctl is-active --quiet multica-backend && backend_was_active=1
  systemctl is-active --quiet multica-frontend && frontend_was_active=1
fi

if [ -z "$cutover_tool" ]; then
  cutover_tool="$release_root/bin/gitlab-vcs-migrate-$EXPECTED_HEAD"
fi
system_hook_attestation_args=()
if [ -n "$system_hook_attestation" ]; then
  system_hook_attestation_args=(--attest-no-legacy-system-hooks-for "$system_hook_attestation")
fi
if ! preflight_json="$("$cutover_tool" preflight "${system_hook_attestation_args[@]}" 2>&1)"; then
  printf '%s\n' "$preflight_json" >&2
  exit 7
fi
printf '%s\n' "$preflight_json"
no_op="$(json_field no_op <<<"$preflight_json")"
preflight_state="$(json_field state <<<"$preflight_json")"
if [ -z "$preflight_state" ]; then
  echo "GitLab preflight returned no migration state; refusing to stop services or change the database." >&2
  exit 7
fi
if [ "$no_op" != "true" ] && [ "$no_op" != "false" ]; then
  echo "GitLab preflight returned no valid no_op result; refusing to stop services or change the database." >&2
  exit 7
fi
if [ "$no_op" = "true" ] && [ "$preflight_state" != "complete" ] && [ "$preflight_state" != "not_initialized" ]; then
  echo "GitLab preflight reports no legacy rows while migration state is $preflight_state; refusing to bypass an unfinished maintenance gate." >&2
  exit 7
fi
if [ "$no_op" = "true" ] && { [ "$cutover_record_status" = "active" ] || [ "$cutover_record_status" = "preparing" ]; }; then
  echo "The pinned GitLab cutover record conflicts with a no-op preflight. Keep services stopped and inspect the database and release state." >&2
  exit 7
fi
if [ "$no_op" = "false" ]; then
  echo "This cutover requires genuine MR, pipeline, job, and note deliveries for every affected project; the release does not synthesize events or use a Hook test trigger. GitLab before 16.11 lacks the project Hook test-trigger API and before 17.3 lacks delivery-history API. Prepare operators and safe real event actions before maintenance; after Hook switch, perform those genuine events while the new endpoint remains gated. Verification stays gated until required receipts are persisted and reconciled; missing evidence can extend downtime." >&2
fi

if [ "$no_op" = "false" ]; then
  prepare_pg_connection
fi
services_stopped=1
sudo systemctl stop multica-frontend multica-backend
if [ "$no_op" = "true" ]; then
  migration_up_attempted=1
  (
    cd server
    "$release_root/bin/migrate" up
  )
  migration_up_completed=1
else
  case "$cutover_record_status" in
    active|preparing)
      cutover_backup_generation="$cutover_generation"
      if [ -z "$cutover_dir" ] || [ -z "$cutover_since" ] || [ ! -s "$cutover_dir/database.dump" ] || [ ! -f "$cutover_dir/artifacts.state" ] || [ ! -x "$cutover_tool" ]; then
        cutover_pending=1
        echo "The pinned cutover backup, artifact snapshot, or tool is missing. Keep services stopped and restore the exact recovery materials before retrying." >&2
        exit 7
      fi
      ;;
    ""|restored)
      if [ "$cutover_record_status" = "restored" ]; then
        cutover_backup_generation=$((cutover_generation + 1))
      else
        cutover_backup_generation=1
      fi
      cutover_generation="$cutover_backup_generation"
      cutover_dir=".gitlab-vcs-cutover-$EXPECTED_COMMIT-$cutover_backup_generation"
      cutover_since="$(date -u "+%Y-%m-%dT%H:%M:%SZ")"
      mkdir -m 0700 -p "$cutover_dir"
      chmod 0700 "$cutover_dir"
      save_cutover_artifacts
      if [ ! -s "$cutover_dir/database.dump" ]; then
        backup_tmp="$cutover_dir/database.dump.tmp.$$"
        rm -f "$backup_tmp"
        if ! run_workspace_pg pg_dump --format=custom --no-owner --no-acl --no-password --file="$backup_tmp" 2>/dev/null; then
          rm -f "$backup_tmp"
          echo "Pre-cutover database backup failed; no GitLab data migration or Hook update was started." >&2
          exit 7
        fi
        test -s "$backup_tmp"
        chmod 0600 "$backup_tmp"
        mv -f "$backup_tmp" "$cutover_dir/database.dump"
      fi
      install -m 0700 "$release_root/bin/gitlab-vcs-migrate-$EXPECTED_HEAD" "$cutover_dir/gitlab-vcs-migrate-$EXPECTED_HEAD"
      cutover_tool="$REMOTE_DIR/$cutover_dir/gitlab-vcs-migrate-$EXPECTED_HEAD"
      write_cutover_record preparing
      cutover_record_head="$EXPECTED_HEAD"
      cutover_record_status=preparing
      ;;
    *)
      echo "GitLab preflight found legacy rows but the durable release state is unsupported. Keep services stopped and resolve the pinned cutover state." >&2
      exit 7
      ;;
  esac
  migration_up_attempted=1
  (
    cd server
    "$release_root/bin/migrate" up
  )
  migration_up_completed=1
fi
if [ "$no_op" = "true" ]; then
  cutover_started=0
  rm -rf "$web_previous" "$bin_previous"
  mkdir -p "$bin_previous"
  for binary in server multica migrate; do
    if [ -f "server/bin/$binary" ]; then cp -p "server/bin/$binary" "$bin_previous/$binary"; fi
  done
  if [ -d apps/web/.next ]; then mv apps/web/.next "$web_previous"; fi
  artifacts_installed=1
  mv "$release_root/.next" apps/web/.next
  install -m 0755 "$release_root/bin/server" server/bin/server
  install -m 0755 "$release_root/bin/multica" server/bin/multica
  install -m 0755 "$release_root/bin/migrate" server/bin/migrate
  sudo systemctl start multica-backend multica-frontend
  systemctl is-active --quiet multica-backend
  systemctl is-active --quiet multica-frontend
  smoke_url="$release_smoke_url"
else
  status_json="$("$cutover_tool" status 2>&1)" || {
    echo "Could not read GitLab migration status after successful preflight." >&2
    exit 7
  }
  read_cli_state "$status_json" || {
    echo "GitLab migration status was not valid JSON; refusing to continue." >&2
    exit 7
  }
  if [ "$cutover_maintenance" != "true" ]; then
    echo "Legacy GitLab data exists but migration state is not maintenance-gated; refusing to deploy." >&2
    exit 7
  fi
  case "$cutover_phase" in
    maintenance|restored|database_migrated|hooks_pending|verification_pending|ready|restoring|restore_blocked) ;;
    *)
      echo "Unsupported GitLab migration phase $cutover_phase; keep services stopped and inspect the cutover state." >&2
      exit 7
      ;;
  esac
  if [ "$cutover_phase" = "restoring" ] || [ "$cutover_phase" = "restore_blocked" ]; then
    echo "Retrying the interrupted Hook/database restore before any new release work." >&2
    cutover_pending=1
    set +e
    restore_cutover_after_failure 7
    restore_status=$?
    set -e
    if [ "$restore_status" -eq 7 ]; then
      trap - EXIT
      echo "The previous release has been restored. This recovery-only invocation did not deploy; redeliver listed GitLab 503 deliveries, then rerun the release if still needed." >&2
    fi
    exit 7
  fi
  if [ "$cutover_record_status" = "preparing" ]; then
    case "$cutover_phase:$cutover_maintenance" in
      maintenance:true)
        if [ "$cutover_generation" != "0" ]; then
          echo "The pre-migration cutover state has an unexpected generation; keep maintenance closed and inspect before continuing." >&2
          exit 7
        fi
        ;;
      restored:true) ;;
      *)
        echo "The pre-migration cutover state is not maintenance-gated or restored (phase=$cutover_phase); keep services stopped and inspect before continuing." >&2
        exit 7
        ;;
    esac
    cutover_generation="$cutover_backup_generation"
    if ! write_cutover_record active; then
      echo "Could not promote the pinned pre-migration recovery record; keep services stopped." >&2
      exit 7
    fi
    cutover_record_status=active
  elif [ "$cutover_record_status" = "active" ]; then
    if [ "$cutover_record_head" != "$EXPECTED_HEAD" ] || [ -z "$cutover_dir" ] || [ -z "$cutover_since" ] || [ ! -s "$cutover_dir/database.dump" ] || [ ! -f "$cutover_dir/artifacts.state" ] || [ ! -x "$cutover_tool" ]; then
      echo "An interrupted GitLab cutover has no matching pinned recovery materials. Keep services stopped and recover it with its original release HEAD." >&2
      exit 7
    fi
  else
    echo "Legacy GitLab data has no durable pre-migration recovery record. Keep services stopped and recover the release snapshot before continuing." >&2
    exit 7
  fi

  if [ "$cutover_phase" = "maintenance" ] || [ "$cutover_phase" = "restored" ]; then
    begin_attempted=1
    if ! cli_output="$("$cutover_tool" begin "${system_hook_attestation_args[@]}" 2>&1)"; then
      printf '%s\n' "$cli_output" >&2
      exit 7
    fi
    printf '%s\n' "$cli_output"
    if ! cli_output="$("$cutover_tool" migrate 2>&1)"; then
      printf '%s\n' "$cli_output" >&2
      exit 7
    fi
  fi
  status_json="$("$cutover_tool" status 2>&1)" || exit 7
  read_cli_state "$status_json" || exit 7
  if [ "$cutover_phase" != "database_migrated" ] && [ "$cutover_phase" != "hooks_pending" ] && [ "$cutover_phase" != "verification_pending" ] && [ "$cutover_phase" != "ready" ]; then
    echo "GitLab database migration is not complete (phase=$cutover_phase); refusing to start the new application." >&2
    exit 7
  fi
  write_cutover_record active
  artifacts_installed=1
  rm -rf apps/web/.next
  cp -a "$release_root/.next" apps/web/.next
  install -m 0755 "$release_root/bin/server" server/bin/server
  install -m 0755 "$release_root/bin/multica" server/bin/multica
  install -m 0755 "$release_root/bin/migrate" server/bin/migrate
  sudo systemctl start multica-backend multica-frontend
  systemctl is-active --quiet multica-backend
  systemctl is-active --quiet multica-frontend
  cutover_public_url="${MULTICA_PUBLIC_URL%/}"
  if [ -z "$cutover_public_url" ]; then
    echo "Workspace .env does not define MULTICA_PUBLIC_URL; keep the cutover gated." >&2
    exit 7
  fi
  curl --noproxy "*" --retry 10 --retry-all-errors --retry-delay 1 --max-time 20 --fail --silent --show-error "$cutover_public_url/" >/dev/null

  if [ "$cutover_phase" = "database_migrated" ] || [ "$cutover_phase" = "hooks_pending" ] || [ "$cutover_phase" = "verification_pending" ]; then
    if ! cli_output="$("$cutover_tool" switch-hooks 2>&1)"; then
      printf '%s\n' "$cli_output" >&2
      status_json="$("$cutover_tool" status 2>&1 || true)"
      phase_after_switch="$(json_field phase <<<"$status_json" 2>/dev/null || true)"
      if [ "$phase_after_switch" = "hooks_pending" ] || [ "$phase_after_switch" = "verification_pending" ]; then
        report_recovery_deliveries "$status_json"
        write_cutover_record active
        cutover_pending=1
        echo "GitLab Hooks remain pending. Configure each manual Hook at the exact workspace-specific URL printed above, then rerun this release at the same pinned HEAD. The runtime remains in maintenance; normal writes and wakeups must stay closed." >&2
        echo "Manual Hook URL format: MULTICA_PUBLIC_URL/api/webhooks/vcs/{workspace-scoped connection UUID}?hook_id={GitLab hook ID}&scope_id={numeric scope ID}&scope_type={project|group|system}" >&2
        exit 7
      fi
      exit 7
    fi
  fi
  status_json="$("$cutover_tool" status 2>&1)" || exit 7
  read_cli_state "$status_json" || exit 7
  capture_recovery_deliveries "$status_json"
  if [ "$cutover_phase" != "verification_pending" ] && [ "$cutover_phase" != "ready" ]; then
    echo "GitLab Hook switch has not reached a verifiable phase (phase=$cutover_phase); keep maintenance enabled." >&2
    exit 7
  fi
  if ! cli_output="$("$cutover_tool" replay --since "$cutover_since" 2>&1)"; then
    printf '%s\n' "$cli_output" >&2
    status_json="$("$cutover_tool" status 2>&1 || true)"
    report_recovery_deliveries "$status_json"
    cutover_pending=1
    echo "GitLab receipt replay is incomplete; keep maintenance enabled and rerun at the same pinned HEAD after resolving the reported issue." >&2
    exit 7
  fi
  status_json="$("$cutover_tool" status 2>&1)" || exit 7
  capture_recovery_deliveries "$status_json"
  if ! cli_output="$("$cutover_tool" verify-hooks --since "$cutover_since" 2>&1)"; then
    printf '%s\n' "$cli_output" >&2
    status_json="$("$cutover_tool" status 2>&1 || true)"
    cutover_phase="$(json_field phase <<<"$status_json" 2>/dev/null || true)"
    report_recovery_deliveries "$status_json"
    if [ "$cutover_phase" = "verification_pending" ] || [ "$cutover_phase" = "ready" ]; then
      cutover_pending=1
      echo "GitLab Hook/delivery verification is pending. On GitLab 13.12.15 only genuine MR, pipeline, job, and note deliveries create receipts; no test-trigger or delivery-history API can fill or enumerate them. Arrange controlled real events, keep maintenance enabled, and rerun replay, verification, and finalization at the same pinned HEAD. Missing evidence means extended downtime; do not bypass the gate." >&2
      exit 7
    fi
    exit 7
  fi
  status_json="$("$cutover_tool" status 2>&1)" || exit 7
  read_cli_state "$status_json" || exit 7
  capture_recovery_deliveries "$status_json"
  if [ "$cutover_phase" != "ready" ] || [ "$cutover_maintenance" != "true" ]; then
    if [ "$cutover_maintenance" = "true" ] && { [ "$cutover_phase" = "verification_pending" ] || [ "$cutover_phase" = "hooks_pending" ]; }; then
      report_recovery_deliveries "$status_json"
      cutover_pending=1
      echo "GitLab receipts or Hook verification remain pending. Keep maintenance enabled; rerun replay, Hook verification, and finalization at the same pinned HEAD." >&2
      exit 7
    fi
    echo "GitLab Hooks and delivery receipts were not confirmed ready; refusing to finalize." >&2
    exit 7
  fi
  if ! cli_output="$("$cutover_tool" finalize 2>&1)"; then
    printf '%s\n' "$cli_output" >&2
    status_json="$("$cutover_tool" status 2>&1 || true)"
    cutover_phase="$(json_field phase <<<"$status_json" 2>/dev/null || true)"
    cutover_maintenance="$(json_field maintenance_mode <<<"$status_json" 2>/dev/null || true)"
    report_recovery_deliveries "$status_json"
    case "$cutover_phase" in
      ready|verification_pending|hooks_pending)
        if [ "$cutover_maintenance" = "true" ]; then
          cutover_pending=1
          echo "Finalization is still gated. Rerun receipt replay, Hook verification, and finalization at the same pinned HEAD; do not open normal writes." >&2
          exit 7
        fi
        ;;
      complete)
        cutover_finalized=1
        if [ "$cutover_maintenance" = "true" ]; then
          echo "Finalization phase is complete but maintenance remains enabled. Do not roll back; keep writes closed and inspect the pinned release state." >&2
        else
          echo "GitLab finalization committed despite its error result; do not roll back. Confirm state and continue only with the pinned new release." >&2
        fi
        exit 7
        ;;
    esac
    exit 7
  fi
  cutover_finalized=1
  status_json="$("$cutover_tool" status 2>&1)" || {
    echo "Finalization reported success; state is not currently readable. Do not roll back or start the old release; keep the pinned new version and inspect migration state." >&2
    exit 7
  }
  read_cli_state "$status_json" || {
    echo "Finalization reported success but status is invalid. Do not roll back or start the old release; keep the pinned new version and inspect migration state." >&2
    exit 7
  }
  capture_recovery_deliveries "$status_json"
  if [ "$cutover_phase" != "complete" ] || [ "$cutover_maintenance" != "false" ]; then
    echo "GitLab finalization did not confirm complete/un-gated state. Do not open normal writes." >&2
    exit 7
  fi
  write_cutover_record complete
  smoke_url="$cutover_public_url"
  report_recovery_deliveries "$status_json"
fi

systemctl is-active --quiet multica-backend
systemctl is-active --quiet multica-frontend
curl --noproxy "*" --retry 10 --retry-all-errors --retry-delay 1 --max-time 20 --fail --silent --show-error "$smoke_url/" >/dev/null
auth_probe_body="$(mktemp)"
auth_probe_status="$(curl --noproxy "*" --max-time 20 --silent --show-error \
  -o "$auth_probe_body" -w "%{http_code}" -H "Content-Type: application/json" \
  -X POST "$smoke_url/auth/send-code" --data "{}")"
if [ "$auth_probe_status" != "400" ] || ! grep -Fq '"email is required"' "$auth_probe_body"; then
  echo "Remote auth smoke failed: expected HTTP 400 email validation, got $auth_probe_status" >&2
  cat "$auth_probe_body" >&2
  rm -f "$auth_probe_body"
  exit 4
fi
rm -f "$auth_probe_body"
release_complete=1
if [ "$cutover_finalized" != "1" ]; then rm -rf "$web_previous" "$bin_previous"; fi
git status --short --branch
git rev-parse HEAD
REMOTE_SCRIPT
)"
  ssh -o RequestTTY=no "$REMOTE_HOST" "REMOTE_DIR=$(printf '%q' "$REMOTE_DIR") REMOTE_NAME=$(printf '%q' "$REMOTE_NAME") BRANCH=$(printf '%q' "$branch") EXPECTED_HEAD=$(printf '%q' "$EXPECTED_HEAD") EXPECTED_COMMIT=$(printf '%q' "$EXPECTED_COMMIT") EXPECTED_BINARY_VERSION=$(printf '%q' "$EXPECTED_BINARY_VERSION") REMOTE_ARTIFACT=$(printf '%q' "$remote_artifact") PUBLIC_URL=$(printf '%q' 'https://multica.zxyh.club') MULTICA_RELEASE_SYSTEM_HOOK_ATTESTATION=$(printf '%q' "$SYSTEM_HOOK_ATTESTATION") bash -s" <<<"$remote_script"
  trap - EXIT
fi

if [ "$SKIP_PACKAGE" != "1" ]; then
  assert_release_head
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
  installed_version="$(/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' /Applications/Multica.app/Contents/Info.plist)"
  if [ "$installed_version" != "$EXPECTED_VERSION" ]; then
    echo "Installed app version $installed_version does not match release $EXPECTED_VERSION" >&2
    exit 3
  fi
  open -a /Applications/Multica.app
fi

assert_release_head

echo "==> Local runtime config"
cat "$HOME/.multica/desktop.json"
