import assert from "node:assert/strict";
import { spawnSync } from "node:child_process";
import { createServer } from "node:net";
import { chmodSync, existsSync, mkdtempSync, mkdirSync, readFileSync, readdirSync, rmSync, statSync, writeFileSync } from "node:fs";
import os from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import test from "node:test";

const scriptDir = path.dirname(fileURLToPath(import.meta.url));
const releaseScript = path.resolve(scriptDir, "../.agents/skills/multica-selfhost-release/scripts/run_release.sh");
const pinnedHead = "abc1234deadbeef" + "0".repeat(25);
const pinnedCommit = "abc1234";
const forwardedHead = "feedfacecafebeef" + "0".repeat(24);
const upstreamPin = "a".repeat(40);
const databaseSecret = "database-password-must-never-be-printed";
const redisSecret = "redis-password-must-never-be-printed";

function shellQuote(value) {
  return "'" + value.replace(/'/g, "'\\''") + "'";
}
function fixture(scenario, databaseUrl = `postgres://release:${databaseSecret}@127.0.0.1:5432/multica?sslmode=require&application_name=release%20test&connect_timeout=3`) {
  const root = mkdtempSync(path.join(os.tmpdir(), "multica-gitlab-release-"));
  const bin = path.join(root, "bin");
  const repo = path.join(root, "repo");
  const remote = path.join(root, "remote");
  const home = path.join(root, "home");
  const state = path.join(root, "state");
  const log = path.join(root, "commands.log");

  mkdirSync(bin, { recursive: true });
  mkdirSync(path.join(repo, "server", "bin"), { recursive: true });
  mkdirSync(path.join(repo, "apps", "web"), { recursive: true });
  mkdirSync(path.join(remote, "server", "bin"), { recursive: true });
  mkdirSync(path.join(remote, "apps", "web", ".next"), { recursive: true });
  mkdirSync(path.join(home, ".multica"), { recursive: true });
  mkdirSync(path.join(state, "services"), { recursive: true });
  mkdirSync(path.join(state, "tmp"), { recursive: true, mode: 0o700 });
  chmodSync(path.join(state, "tmp"), 0o700);
  writeFileSync(path.join(home, ".multica", "desktop.json"), "{}\n");
  writeFileSync(path.join(repo, "apps", "web", "next-env.d.ts"), "// fixture\n");
  const workspaceEnv = [
    `DATABASE_URL=${shellQuote(databaseUrl)}`,
    "MULTICA_VCS_INTEGRATION_ENABLED=true",
    "MULTICA_VCS_SECRET_KEY=test-encryption-key-not-for-output",
    "MULTICA_PUBLIC_URL=https://selfhost.example.test",
  ];
  if (["redis_legacy", "redis_empty", "redis_partial", "real_pg_probe"].includes(scenario)) workspaceEnv.push(`REDIS_URL=redis://relay:${redisSecret}@127.0.0.1/0`);
  writeFileSync(path.join(remote, ".env"), `${workspaceEnv.join("\n")}\n`, { mode: 0o600 });
  chmodSync(path.join(remote, ".env"), 0o600);
  writeFileSync(path.join(remote, "apps", "web", ".next", "BUILD_ID"), "old-build\n");
  writeFileSync(path.join(remote, "apps", "web", ".next", "routes-manifest.json"), "{}\n");
  writeFileSync(path.join(remote, "apps", "web", ".next", "prerender-manifest.json"), "{}\n");
  for (const name of ["server", "multica", "migrate"]) {
    writeFileSync(path.join(remote, "server", "bin", name), `old ${name}\n`, { mode: 0o755 });
  }
  writeFileSync(log, "");

  const put = (name, source) => {
    const file = path.join(bin, name);
    writeFileSync(file, source);
    chmodSync(file, 0o755);
  };

  put("git", `#!/usr/bin/env bash
set -euo pipefail
printf 'git cwd=%s %s\\n' "$PWD" "$*" >>"$MULTICA_TEST_LOG"
if [[ "$PWD" == "$MULTICA_TEST_REMOTE_DIR"* ]]; then
  case "$*" in
    "fetch $MULTICA_TEST_REMOTE_NAME $MULTICA_TEST_BRANCH") ;;
    "rev-parse --abbrev-ref HEAD") echo "$MULTICA_TEST_BRANCH" ;;
    "status --porcelain")
      case "$MULTICA_TEST_SCENARIO" in
        restored_new_head|restored_unknown_dir)
          printf '?? .gitlab-vcs-cutover.state\\n?? %s/\\n' "$MULTICA_TEST_RESTORED_DIR"
          if [ "$MULTICA_TEST_SCENARIO" = "restored_unknown_dir" ]; then printf '?? %s/\\n' "$MULTICA_TEST_UNKNOWN_CUTOVER_DIR"; fi
          ;;
      esac
      ;;
    "restore apps/web/next-env.d.ts") ;;
    "merge --ff-only $MULTICA_TEST_REMOTE_NAME/$MULTICA_TEST_BRANCH") ;;
    "rev-parse HEAD") echo "$MULTICA_TEST_HEAD" ;;
    "rev-parse --short HEAD") echo "$MULTICA_TEST_COMMIT" ;;
    "status --short --branch") echo "## $MULTICA_TEST_BRANCH" ;;
    *) echo "unexpected remote git $*" >&2; exit 9 ;;
  esac
else
  case "$*" in
    "rev-parse --abbrev-ref HEAD") echo "$MULTICA_TEST_BRANCH" ;;
    "status --porcelain"|"status --porcelain -- apps/web/next-env.d.ts") ;;
    "restore apps/web/next-env.d.ts") ;;
    "fetch upstream main --tags") ;;
    "ls-remote --exit-code --heads origin $MULTICA_TEST_BRANCH") exit 0 ;;
    "fetch origin $MULTICA_TEST_BRANCH") ;;
    "rev-parse --verify $MULTICA_TEST_UPSTREAM_PIN^{commit}")
      if [ "$MULTICA_TEST_SCENARIO" = pin_missing_commit ]; then exit 1; fi
      echo "$MULTICA_TEST_UPSTREAM_PIN"
      ;;
    "rev-list --count HEAD..origin/$MULTICA_TEST_BRANCH")
      if [ "$MULTICA_TEST_SCENARIO" = pin_origin_ahead ]; then echo 1; else echo 0; fi
      ;;
    "merge-base --is-ancestor $MULTICA_TEST_UPSTREAM_PIN HEAD")
      if [ "$MULTICA_TEST_SCENARIO" = pin_not_ancestor ]; then exit 1; fi
      exit 0
      ;;
    "merge-base --is-ancestor upstream/main HEAD") exit 1 ;;
    "merge --no-ff upstream/main -m chore: merge upstream/main") ;;
    "push origin $MULTICA_TEST_BRANCH") ;;
    "describe --tags --always --dirty") echo "v0.0.0-test" ;;
    "rev-parse HEAD") echo "$MULTICA_TEST_HEAD" ;;
    "rev-parse --short HEAD") echo "$MULTICA_TEST_COMMIT" ;;
    *) echo "unexpected local git $*" >&2; exit 9 ;;
  esac
fi
`);

  put("go", `#!/usr/bin/env bash
set -euo pipefail
printf 'go cwd=%s %s\\n' "$PWD" "$*" >>"$MULTICA_TEST_LOG"
out=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "-o" ]; then out="$arg"; break; fi
  prev="$arg"
done
[ -n "$out" ] || { echo "go build missing -o" >&2; exit 9; }
mkdir -p "$(dirname "$out")"
name="$(basename "$out")"
case "$name" in
  server|multica) printf '#!/usr/bin/env bash\\necho \\\"release binary\\\"\\n' >"$out" ;;
  migrate) cat >"$out" <<'BIN'
#!/usr/bin/env bash
set -euo pipefail
printf 'migrate %s\\n' "$*" >>"$MULTICA_TEST_LOG"
if [ "$1" = up ]; then
  if [ "$MULTICA_TEST_SCENARIO" = migration_up_fails ]; then
    echo "mock migration failed before creating cutover state" >&2
    exit 16
  fi
  if [ "$MULTICA_TEST_SCENARIO" = ordinary ] || [ "$MULTICA_TEST_SCENARIO" = redis_empty ] || [ "$MULTICA_TEST_SCENARIO" = complete_noop ]; then
    printf 'complete\\nfalse\\n0\\nfalse\\n' >"$MULTICA_TEST_STATE_DIR/state"
  elif [ "$MULTICA_TEST_SCENARIO" = restore_failure ]; then
    printf 'maintenance\\ntrue\\n0\\nfalse\\n' >"$MULTICA_TEST_STATE_DIR/state"
  else
    printf 'maintenance\\ntrue\\n0\\ntrue\\n' >"$MULTICA_TEST_STATE_DIR/state"
  fi
fi
BIN
    ;;
  gitlab-vcs-migrate*) cat >"$out" <<'BIN'
#!/usr/bin/env bash
set -euo pipefail
cmd="$1"
printf 'gitlab-vcs-migrate %s\\n' "$*" >>"$MULTICA_TEST_LOG"
state_file="$MULTICA_TEST_STATE_DIR/state"
read_state() { IFS= read -r phase <"$state_file"; sed -n '2p' "$state_file"; }
write_state() {
  local payload
  payload="$(sed -n '4p' "$state_file")"
  if [ "$#" -ge 4 ]; then payload="$4"; fi
  printf '%s\\n%s\\n%s\\n%s\\n' "$1" "$2" "$3" "$payload" >"$state_file"
}
status_json() {
  phase="$(sed -n '1p' "$state_file")"
  maintenance="$(sed -n '2p' "$state_file")"
  generation="$(sed -n '3p' "$state_file")"
  payload="$(sed -n '4p' "$state_file")"
  ids='[]'
  [ "$payload" != true ] || ids='["delivery-1","delivery-2"]'
  printf '{\\n  "phase": "%s",\\n  "maintenance_mode": %s,\\n  "generation": %s,\\n  "recovery_delivery_ids": %s,\\n  "last_error": ""\\n}\\n' "$phase" "$maintenance" "$generation" "$ids"
}
case "$cmd" in
  preflight)
    if [ "$MULTICA_TEST_SCENARIO" = "complete_noop" ] || [ -f "$state_file" ]; then
      printf 'gitlab-vcs-migrate preflight state_table=present\\n' >>"$MULTICA_TEST_LOG"
    else
      printf 'gitlab-vcs-migrate preflight state_table=absent\\n' >>"$MULTICA_TEST_LOG"
    fi
    if [ "$MULTICA_TEST_SCENARIO" = "maintenance_empty" ]; then
      printf '{\\n  "no_op": true,\\n  "state": "maintenance",\\n  "manual_hooks": 0,\\n  "findings": []\\n}\\n'
    elif [ "$MULTICA_TEST_SCENARIO" = "complete_noop" ]; then
      printf '{\\n  "no_op": true,\\n  "state": "complete",\\n  "manual_hooks": 0,\\n  "findings": []\\n}\\n'
    elif [ "$MULTICA_TEST_SCENARIO" = ordinary ] || [ "$MULTICA_TEST_SCENARIO" = redis_empty ]; then
      printf '{\\n  "no_op": true,\\n  "state": "not_initialized",\\n  "manual_hooks": 0,\\n  "findings": []\\n}\\n'
    else
      printf '{\\n  "no_op": false,\\n  "state": "maintenance",\\n  "manual_hooks": 1,\\n  "findings": [{"code":"manual_hook_review","severity":"warning"}]\\n}\\n'
    fi
    ;;
  status)
    [ -f "$state_file" ] || { echo "migration state table is absent" >&2; exit 42; }
    status_json
    ;;
  begin)
    phase="$(sed -n '1p' "$state_file")"; generation="$(sed -n '3p' "$state_file")"
    [ "$generation" -gt 0 ] || generation=1
    write_state "$phase" true "$generation"
    status_json
    ;;
  migrate)
    if [ "$MULTICA_TEST_SCENARIO" = restore_failure ]; then exit 17; fi
    write_state database_migrated true "$(sed -n '3p' "$state_file")"
    status_json
    ;;
  switch-hooks)
    if [ "$MULTICA_TEST_SCENARIO" = manual_pending ]; then
      write_state hooks_pending true "$(sed -n '3p' "$state_file")"
      echo "manual project Hook must use https://selfhost.example.test/api/webhooks/vcs/connection-id?hook_id=22&scope_id=77&scope_type=project" >&2
      exit 18
    fi
    write_state verification_pending true "$(sed -n '3p' "$state_file")"
    status_json
    ;;
  replay)
    write_state "$(sed -n '1p' "$state_file")" "$(sed -n '2p' "$state_file")" "$(sed -n '3p' "$state_file")" false
    status_json
    ;;
  verify-hooks)
    if [ "$MULTICA_TEST_SCENARIO" = receipts_missing ]; then
      echo "enabled event types without real reconciled deliveries: merge_request, pipeline, job, note" >&2
      exit 22
    fi
    write_state ready true "$(sed -n '3p' "$state_file")"
    status_json
    ;;
  finalize)
    write_state complete false "$(sed -n '3p' "$state_file")" false
    status_json
    ;;
  restore)
    if [ "$MULTICA_TEST_SCENARIO" = restore_failure ]; then
      write_state restore_blocked true "$(sed -n '3p' "$state_file")"
      echo "restore blocked" >&2
      exit 19
    fi
    write_state restored true "$(sed -n '3p' "$state_file")"
    status_json
    ;;
  *) echo "unexpected GitLab migration command $cmd" >&2; exit 9 ;;
esac
BIN
    ;;
  *) echo "unexpected go output $name" >&2; exit 9 ;;
esac
chmod 0755 "$out"
`);

  put("pnpm", `#!/usr/bin/env bash
set -euo pipefail
printf 'pnpm cwd=%s %s\\n' "$PWD" "$*" >>"$MULTICA_TEST_LOG"
case "$*" in
  "--filter @multica/web typecheck") ;;
  "--filter @multica/web build")
    mkdir -p apps/web/.next
    printf 'new-build\\n' >apps/web/.next/BUILD_ID
    printf '{}\\n' >apps/web/.next/routes-manifest.json
    printf '{}\\n' >apps/web/.next/prerender-manifest.json
    ;;
  "install --frozen-lockfile") ;;
  *) echo "unexpected pnpm $*" >&2; exit 9 ;;
esac
`);

  put("scp", `#!/usr/bin/env bash
set -euo pipefail
printf 'scp %s\\n' "$*" >>"$MULTICA_TEST_LOG"
`);

  put("ssh", `#!/usr/bin/env bash
set -euo pipefail
printf 'ssh %s\\n' "$*" >>"$MULTICA_TEST_LOG"
command="${'${!#}'}"
script="$(cat)"
printf '%s\\n' "$script" | /bin/bash -c "$command"
`);

  put("tar", `#!/usr/bin/env bash
set -euo pipefail
printf 'tar %s\\n' "$*" >>"$MULTICA_TEST_LOG"
archive=""; destination=""; previous=""
for arg in "$@"; do
  if [ "$previous" = "-czf" ]; then archive="$arg"; fi
  if [ "$previous" = "-C" ]; then destination="$arg"; fi
  previous="$arg"
done
if [ -n "$archive" ]; then : >"$archive"; fi
if [ -n "$destination" ]; then
  mkdir -p "$destination/.next"
  printf 'new-build\\n' >"$destination/.next/BUILD_ID"
  printf '{}\\n' >"$destination/.next/routes-manifest.json"
  printf '{}\\n' >"$destination/.next/prerender-manifest.json"
fi
`);

  put("assert-pg-connection", `#!/usr/bin/env node
const fs = require("node:fs");
const fail = () => process.exit(1);
try {
  const servicePath = process.env.PGSERVICEFILE;
  if (!servicePath || process.env.PGSERVICE !== "multica_cutover" || process.env.DATABASE_URL) fail();
  if ((fs.statSync(servicePath).mode & 0o777) !== 0o600) fail();
  const service = fs.readFileSync(servicePath, "utf8");
  const hasLine = (key, value) => service.split("\\n").includes(key + "=" + value);
  if (!hasLine("host", process.env.MULTICA_TEST_DBHOST)) fail();
  if (!hasLine("port", process.env.MULTICA_TEST_DBPORT)) fail();
  if (!hasLine("user", process.env.MULTICA_TEST_DBUSER)) fail();
  if (!hasLine("dbname", process.env.MULTICA_TEST_DBNAME)) fail();
  if (!hasLine("sslmode", process.env.MULTICA_TEST_SSLMODE)) fail();
  if (!hasLine("application_name", process.env.MULTICA_TEST_APPNAME)) fail();
  if (!hasLine("connect_timeout", process.env.MULTICA_TEST_CONNECT_TIMEOUT)) fail();
  if (service.includes(process.env.MULTICA_TEST_DB_PASSWORD)) fail();
  const passfile = service.match(/^passfile=(.+)$/m)?.[1];
  if (!passfile || (fs.statSync(passfile).mode & 0o777) !== 0o600) fail();
  const escaped = process.env.MULTICA_TEST_DB_PASSWORD.replace(/\\\\/g, "\\\\\\\\").replace(/:/g, "\\\\:");
  if (fs.readFileSync(passfile, "utf8") !== "*:*:*:*:" + escaped + "\\n") fail();
} catch {
  fail();
}
`);

  put("pg_dump", `#!/usr/bin/env bash
set -euo pipefail
[[ "$*" != *"://"* && "$*" != *"$MULTICA_TEST_DB_PASSWORD"* ]] || { echo "database credentials reached pg_dump argv" >&2; exit 9; }
printf 'pg_dump %s\\n' "$*" >>"$MULTICA_TEST_LOG"
assert-pg-connection
printf 'pg_dump libpq configuration secure\\n' >>"$MULTICA_TEST_LOG"
if [ "$MULTICA_REAL_PG" = "1" ]; then unset MULTICA_TEST_DB_PASSWORD; exec "$MULTICA_REAL_PG_DUMP" "$@"; fi
file=""
for arg in "$@"; do case "$arg" in --file=*) file="${'${arg#--file=}'}" ;; esac; done
[ -n "$file" ] || { echo "missing backup file" >&2; exit 9; }
printf 'mock database backup\\n' >"$file"
`);

  put("psql", `#!/usr/bin/env bash
set -euo pipefail
[[ "$*" != *"://"* && "$*" != *"$MULTICA_TEST_DB_PASSWORD"* ]] || { echo "database credentials reached psql argv" >&2; exit 9; }
printf 'psql %s\\n' "$*" >>"$MULTICA_TEST_LOG"
assert-pg-connection
printf 'psql libpq configuration secure\\n' >>"$MULTICA_TEST_LOG"
if [ "$MULTICA_REAL_PG" = "1" ]; then unset MULTICA_TEST_DB_PASSWORD; exec "$MULTICA_REAL_PSQL" "$@"; fi
case "$MULTICA_TEST_SCENARIO" in
  redis_legacy) printf 't\\n' ;;
  redis_partial) printf 'partial\\n' ;;
  *) printf 'f\\n' ;;
esac
`);
  put("sudo", `#!/usr/bin/env bash
set -euo pipefail
printf 'sudo %s\\n' "$*" >>"$MULTICA_TEST_LOG"
"$@"
`);

  put("systemctl", `#!/usr/bin/env bash
set -euo pipefail
printf 'systemctl %s\\n' "$*" >>"$MULTICA_TEST_LOG"
state_dir="$MULTICA_TEST_STATE_DIR/services"
mkdir -p "$state_dir"
case "$1" in
  is-active)
    service="${'${@: -1}'}"
    [ "$(cat "$state_dir/$service" 2>/dev/null || echo inactive)" = active ]
    ;;
  stop|start)
    action="$1"; shift
    for service in "$@"; do printf '%s\\n' "$action" >/dev/null; if [ "$action" = start ]; then printf active >"$state_dir/$service"; else printf inactive >"$state_dir/$service"; fi; done
    ;;
  *) echo "unexpected systemctl $*" >&2; exit 9 ;;
esac
`);

  put("curl", `#!/usr/bin/env bash
set -euo pipefail
printf 'curl %s\\n' "$*" >>"$MULTICA_TEST_LOG"
body_file=""
for ((i=1; i<=$#; i++)); do
  arg="${'${!i}'}"
  if [ "$arg" = -o ]; then j=$((i+1)); body_file="${'${!j}'}"; fi
done
if [[ "$*" == *"/auth/send-code"* ]]; then
  [ -z "$body_file" ] || printf '{"error":"email is required"}\\n' >"$body_file"
  printf 400
else
  printf 200
fi
`);

  let restoredCutoverDir = "";
  let unknownCutoverDir = "";
  for (const service of ["multica-backend", "multica-frontend"]) {
    writeFileSync(path.join(state, "services", service), "active");
  }
  if (["restore_retry", "new_commit_restore_active", "resume_preparing_attested", "restored_new_head", "restored_unknown_dir"].includes(scenario)) {
    const cutoverDir = `.gitlab-vcs-cutover-${pinnedCommit}-1`;
    const cutoverPath = path.join(remote, cutoverDir);
    const restoredScenario = ["restored_new_head", "restored_unknown_dir"].includes(scenario);
    if (restoredScenario) restoredCutoverDir = cutoverDir;
    mkdirSync(cutoverPath, { recursive: true });
    if (scenario === "resume_preparing_attested" || restoredScenario) {
      writeFileSync(path.join(cutoverPath, "database.dump"), "mock database backup", { mode: 0o600 });
    }
    if (scenario === "restored_unknown_dir") {
      unknownCutoverDir = `.gitlab-vcs-cutover-${pinnedCommit}-nearby`;
      const unknownPath = path.join(remote, unknownCutoverDir);
      mkdirSync(unknownPath, { recursive: true });
      writeFileSync(path.join(unknownPath, "unrelated.txt"), "preserve me");
    }
    const artifacts = path.join(cutoverPath, "artifacts");
    const oldWeb = path.join(artifacts, "web.previous");
    mkdirSync(oldWeb, { recursive: true });
    writeFileSync(path.join(oldWeb, "BUILD_ID"), "old-build\n");
    writeFileSync(path.join(oldWeb, "routes-manifest.json"), "{}\n");
    writeFileSync(path.join(oldWeb, "prerender-manifest.json"), "{}\n");
    writeFileSync(path.join(artifacts, "server.previous"), "old server\n");
    writeFileSync(path.join(artifacts, "multica.previous"), "old multica\n");
    writeFileSync(path.join(artifacts, "migrate.previous"), "old migrate\n");
    writeFileSync(path.join(cutoverPath, "artifacts.state"), "web=1\nserver=1\nmultica=1\nmigrate=1\n", { mode: 0o600 });
    writeFileSync(path.join(remote, ".gitlab-vcs-cutover.state"), [
      `head=${pinnedHead}`,
      `status=${scenario === "resume_preparing_attested" ? "preparing" : restoredScenario ? "restored" : "active"}`,
      `backup_dir=${cutoverDir}`,
      "since=2026-11-23T00:00:00Z",
      "generation=1",
      ...(scenario === "resume_preparing_attested" ? ["system_hook_attestation=https://gitlab.example.test"] : []),
      "backend_was_active=1",
      "frontend_was_active=1",
      "",
    ].join("\n"), { mode: 0o600 });
    const resumePreparing = scenario === "resume_preparing_attested";
    const restorePhase = resumePreparing ? "maintenance" : scenario === "new_commit_restore_active" || restoredScenario ? "restored" : "restore_blocked";
    const migrationGeneration = resumePreparing ? 0 : 1;
    writeFileSync(path.join(state, "state"), `${restorePhase}\ntrue\n${migrationGeneration}\ntrue\n`);
    writeFileSync(path.join(remote, "apps", "web", ".next", "BUILD_ID"), "new-build\n");
    for (const name of ["server", "multica", "migrate"]) {
      writeFileSync(path.join(remote, "server", "bin", name), `new ${name}\n`, { mode: 0o755 });
    }
    const recoveryTool = path.join(cutoverPath, `gitlab-vcs-migrate-${pinnedHead}`);
    writeFileSync(recoveryTool, `#!/usr/bin/env bash
set -euo pipefail
cmd="$1"
state_file="$MULTICA_TEST_STATE_DIR/state"
printf 'gitlab-vcs-migrate %s\\n' "$*" >>"$MULTICA_TEST_LOG"
status_json() {
  phase="$(sed -n '1p' "$state_file")"
  maintenance="$(sed -n '2p' "$state_file")"
  generation="$(sed -n '3p' "$state_file")"
  printf '{\\n  "phase": "%s",\\n  "maintenance_mode": %s,\\n  "generation": %s,\\n  "recovery_delivery_ids": ["delivery-1"],\\n  "last_error": ""\\n}\\n' "$phase" "$maintenance" "$generation"
}
write_state() {
  printf '%s\\n%s\\n%s\\n%s\\n' "$1" "$2" "$3" "$4" >"$state_file"
}
case "$cmd" in
  preflight)
    printf 'gitlab-vcs-migrate preflight state_table=present\\n' >>"$MULTICA_TEST_LOG"
    printf '{\\n  "no_op": false,\\n  "state": "maintenance",\\n  "manual_hooks": 1,\\n  "findings": []\\n}\\n'
    ;;
  status) status_json ;;
  begin)
    generation="$(sed -n '3p' "$state_file")"
    [ "$generation" -gt 0 ] || generation=1
    write_state maintenance true "$generation" true
    status_json
    ;;
  migrate)
    write_state database_migrated true "$(sed -n '3p' "$state_file")" true
    status_json
    ;;
  switch-hooks)
    write_state verification_pending true "$(sed -n '3p' "$state_file")" true
    status_json
    ;;
  replay)
    write_state "$(sed -n '1p' "$state_file")" "$(sed -n '2p' "$state_file")" "$(sed -n '3p' "$state_file")" false
    status_json
    ;;
  verify-hooks)
    write_state ready true "$(sed -n '3p' "$state_file")" false
    status_json
    ;;
  finalize)
    write_state complete false "$(sed -n '3p' "$state_file")" false
    status_json
    ;;
  restore)
    write_state restored true 1 true
    echo "restored"
    ;;
  *) echo "unexpected recovery command $cmd" >&2; exit 9 ;;
esac
`);
    chmodSync(recoveryTool, 0o700);
  }

  const env = {
    ...process.env,
    PATH: `${bin}${path.delimiter}${process.env.PATH}`,
    HOME: home,
    MULTICA_REPO: repo,
    MULTICA_REMOTE_HOST: "mock-remote",
    MULTICA_REMOTE_DIR: remote,
    MULTICA_REMOTE_NAME: "origin",
    MULTICA_UPSTREAM_SYNC_STRATEGY: "merge",
    MULTICA_UPSTREAM_PIN: "",
    TMPDIR: path.join(state, "tmp"),
    MULTICA_SKIP_CLI_INSTALL: "1",
    MULTICA_SKIP_PACKAGE: "1",
    MULTICA_SKIP_INSTALL: "1",
    MULTICA_TEST_LOG: log,
    MULTICA_TEST_STATE_DIR: state,
    MULTICA_TEST_REMOTE_DIR: remote,
    MULTICA_TEST_REMOTE_NAME: "origin",
    MULTICA_TEST_BRANCH: "feature/gitlab-cutover",
    MULTICA_TEST_HEAD: ["new_commit_restore_active", "restored_new_head", "restored_unknown_dir"].includes(scenario) ? forwardedHead : pinnedHead,
    MULTICA_TEST_COMMIT: ["new_commit_restore_active", "restored_new_head", "restored_unknown_dir"].includes(scenario) ? "feedface" : pinnedCommit,
    MULTICA_TEST_UPSTREAM_PIN: upstreamPin,
    MULTICA_TEST_SCENARIO: scenario,
    MULTICA_TEST_RESTORED_DIR: restoredCutoverDir,
    MULTICA_TEST_UNKNOWN_CUTOVER_DIR: unknownCutoverDir,
    MULTICA_TEST_DB_PASSWORD: databaseSecret,
    MULTICA_TEST_DBHOST: "127.0.0.1",
    MULTICA_TEST_DBPORT: "5432",
    MULTICA_TEST_DBUSER: "release",
    MULTICA_TEST_DBNAME: "multica",
    MULTICA_TEST_SSLMODE: "require",
    MULTICA_TEST_APPNAME: "release test",
    MULTICA_TEST_CONNECT_TIMEOUT: "3",
    MULTICA_REAL_PG: "0",
  };
  return { root, bin, repo, remote, home, state, log, env };
}

function withFixture(t, scenario, databaseUrl) {
  const fx = fixture(scenario, databaseUrl);
  t.after(() => rmSync(fx.root, { recursive: true, force: true }));
  return fx;
}
function runRelease(fx) {
  return spawnSync("bash", [releaseScript], { cwd: fx.repo, env: fx.env, encoding: "utf8" });
}

function log(fx) {
  return readFileSync(fx.log, "utf8").trimEnd().split("\n").filter(Boolean);
}

function hasCommand(records, fragment) {
  return records.some((record) => record.includes(fragment));
}

function assertBefore(records, first, second) {
  const a = records.findIndex((record) => record.includes(first));
  const b = records.findIndex((record) => record.includes(second));
  assert.notEqual(a, -1, `missing command: ${first}`);
  assert.notEqual(b, -1, `missing command: ${second}`);
  assert.ok(a < b, `expected ${first} before ${second}`);
}

test("legacy cutover backs up, gates writes, verifies receipts, and finalizes", (t) => {
  const fx = withFixture(t, "success");
  const result = runRelease(fx);
  assert.equal(result.status, 0, result.stderr);
  const commands = log(fx);
  for (const step of [
    "migrate up",
    "gitlab-vcs-migrate preflight",
    "pg_dump",
    "gitlab-vcs-migrate begin",
    "gitlab-vcs-migrate migrate",
    "systemctl start multica-backend multica-frontend",
    "gitlab-vcs-migrate switch-hooks",
    "gitlab-vcs-migrate replay",
    "gitlab-vcs-migrate verify-hooks",
    "gitlab-vcs-migrate finalize",
  ]) assert.ok(hasCommand(commands, step), `missing ${step}`);
  assertBefore(commands, "gitlab-vcs-migrate preflight state_table=absent", "systemctl stop multica-frontend multica-backend");
  assertBefore(commands, "gitlab-vcs-migrate preflight", "pg_dump");
  assertBefore(commands, "systemctl stop multica-frontend multica-backend", "pg_dump");
  assertBefore(commands, "pg_dump", "migrate up");
  assertBefore(commands, "migrate up", "gitlab-vcs-migrate status");
  assertBefore(commands, "gitlab-vcs-migrate migrate", "systemctl start multica-backend multica-frontend");
  assertBefore(commands, "systemctl start multica-backend multica-frontend", "gitlab-vcs-migrate switch-hooks");
  assertBefore(commands, "gitlab-vcs-migrate replay", "gitlab-vcs-migrate verify-hooks");
  assertBefore(commands, "gitlab-vcs-migrate verify-hooks", "gitlab-vcs-migrate finalize");
  assert.ok(hasCommand(commands, `gitlab-vcs-migrate-${pinnedHead}`), "migration helper is built from the pinned HEAD");
  assert.equal(readFileSync(path.join(fx.state, "state"), "utf8").split("\n").slice(0, 2).join(" "), "complete false");
  assert.equal(readFileSync(path.join(fx.state, "services", "multica-backend"), "utf8"), "active");
  assert.equal(readFileSync(path.join(fx.state, "services", "multica-frontend"), "utf8"), "active");
  assert.ok(`${result.stdout}\n${result.stderr}`.includes("delivery-1"));
  assert.ok(`${result.stdout}\n${result.stderr}`.includes("delivery-2"));
  assert.ok(hasCommand(commands, "pg_dump libpq configuration secure"));
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(databaseSecret));
  const backups = [];
  function visit(dir) {
    for (const entry of readdirSync(dir, { withFileTypes: true })) {
      const full = path.join(dir, entry.name);
      if (entry.isDirectory()) visit(full);
      else if (entry.name.endsWith(".dump")) backups.push(full);
    }
  }
  visit(fx.remote);
  assert.equal(backups.length, 1, "one private database backup is retained");
  assert.equal(statSync(backups[0]).mode & 0o777, 0o600);
  assert.deepEqual(readdirSync(path.join(fx.state, "tmp")), [], "temporary libpq credential files are removed");
});
test("failed schema migration leaves a pinned snapshot and same-HEAD retry resumes safely", (t) => {
  const fx = withFixture(t, "migration_up_fails");
  const first = runRelease(fx);
  assert.notEqual(first.status, 0);
  const firstCommands = log(fx);
  assertBefore(firstCommands, "gitlab-vcs-migrate preflight state_table=absent", "pg_dump");
  assertBefore(firstCommands, "pg_dump", "migrate up");
  assert.ok(!hasCommand(firstCommands, "gitlab-vcs-migrate status"), "strict status must not run while the migration state table is absent");
  assert.ok(!hasCommand(firstCommands, "systemctl start"), "services remain stopped when schema migration is incomplete");
  assert.match(`${first.stdout}\n${first.stderr}`, /Do not query cutover status.*keep services stopped/i);
  assert.equal(readFileSync(path.join(fx.remote, ".gitlab-vcs-cutover.state"), "utf8").includes("status=preparing"), true);
  assert.equal(readFileSync(path.join(fx.state, "services", "multica-backend"), "utf8"), "inactive");
  assert.equal(readFileSync(path.join(fx.state, "services", "multica-frontend"), "utf8"), "inactive");

  fx.env.MULTICA_TEST_SCENARIO = "success";
  const retry = runRelease(fx);
  assert.equal(retry.status, 0, retry.stderr);
  const retryCommands = log(fx).slice(firstCommands.length);
  assertBefore(retryCommands, "gitlab-vcs-migrate preflight state_table=absent", "migrate up");
  assertBefore(retryCommands, "migrate up", "gitlab-vcs-migrate status");
  assert.ok(!hasCommand(retryCommands, "pg_dump"), "retry reuses the original pre-migration database snapshot");
  assert.equal(readFileSync(path.join(fx.state, "state"), "utf8").split("\n").slice(0, 2).join(" "), "complete false");
});

test("manual Hooks remain gated and resumable without replay or finalize", (t) => {
  const fx = withFixture(t, "manual_pending");
  const result = runRelease(fx);
  assert.notEqual(result.status, 0, "a pending cutover is not a completed release");
  const commands = log(fx);
  assert.ok(hasCommand(commands, "gitlab-vcs-migrate switch-hooks"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate replay"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate verify-hooks"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate finalize"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate restore"));
  assert.match(`${result.stdout}\n${result.stderr}`, /manual Hook|maintenance/i);
  assert.match(`${result.stdout}\n${result.stderr}`, /api\/webhooks\/vcs/);
  assert.equal(readFileSync(path.join(fx.state, "state"), "utf8").split("\n").slice(0, 2).join(" "), "hooks_pending true");
  assert.equal(readFileSync(path.join(fx.state, "services", "multica-backend"), "utf8"), "active");
  assert.ok(`${result.stdout}\n${result.stderr}`.includes("delivery-1"));
  assert.ok(!hasCommand(commands, "curl --noproxy \"*\" --max-time 20 --silent --show-error"), "normal-write smoke must wait until finalization");
});
test("missing real event receipts stay gated on GitLab 13.12 without synthetic verification", (t) => {
  const fx = withFixture(t, "receipts_missing");
  const result = runRelease(fx);
  assert.notEqual(result.status, 0);
  const commands = log(fx);
  assert.ok(hasCommand(commands, "gitlab-vcs-migrate verify-hooks"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate finalize"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate restore"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate test"));
  assert.match(`${result.stdout}\n${result.stderr}`, /genuine MR, pipeline, job, and note deliveries|delivery-history API/i);
  assert.equal(readFileSync(path.join(fx.state, "state"), "utf8").split("\n").slice(0, 2).join(" "), "verification_pending true");
  assert.equal(readFileSync(path.join(fx.state, "services", "multica-backend"), "utf8"), "active");
  assert.ok(!hasCommand(commands, "curl --noproxy \"*\" --max-time 20 --silent --show-error"), "normal-write smoke remains blocked");
});

test("failed data rollback keeps services stopped and leaves explicit maintenance instructions", (t) => {
  const fx = withFixture(t, "restore_failure");
  const result = runRelease(fx);
  assert.notEqual(result.status, 0);
  const commands = log(fx);
  assert.ok(hasCommand(commands, "gitlab-vcs-migrate restore"), "the release attempts the CLI's full Hook+DB restore");
  assert.equal(readFileSync(path.join(fx.state, "state"), "utf8").split("\n").slice(0, 2).join(" "), "restore_blocked true");
  assert.ok(!hasCommand(commands, "systemctl start multica-backend multica-frontend"), "old or normal-write services must not restart");
  assert.match(`${result.stdout}\n${result.stderr}`, /do not start|keep.*maintenance|restore.*blocked/i);
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(databaseSecret));
});

test("ordinary no-legacy release keeps the existing migration/restart path", (t) => {
  const fx = withFixture(t, "ordinary");
  const result = runRelease(fx);
  assert.equal(result.status, 0, result.stderr);
  const commands = log(fx);
  assert.ok(hasCommand(commands, "fetch upstream main --tags"));
  assert.ok(hasCommand(commands, "merge --no-ff upstream/main"));
  assert.ok(hasCommand(commands, "migrate up"));
  assert.ok(hasCommand(commands, "gitlab-vcs-migrate preflight"));
  assertBefore(commands, "gitlab-vcs-migrate preflight state_table=absent", "migrate up");
  assertBefore(commands, "gitlab-vcs-migrate preflight state_table=absent", "systemctl stop multica-frontend multica-backend");
  assertBefore(commands, "systemctl stop multica-frontend multica-backend", "migrate up");
  for (const operation of ["begin", "migrate", "switch-hooks", "replay", "verify-hooks", "finalize", "restore"]) {
    assert.ok(!hasCommand(commands, `gitlab-vcs-migrate ${operation}`), `${operation} must not run without legacy rows`);
  }
  assert.ok(!hasCommand(commands, "pg_dump"), "ordinary releases do not add a production backup operation");
  assert.ok(hasCommand(commands, "systemctl start multica-backend multica-frontend"));
  assert.ok(hasCommand(commands, "/auth/send-code"), "the usual post-release smoke remains enabled");
  assert.ok(!existsSync(path.join(fx.remote, ".gitlab-vcs-cutover-state")));
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(databaseSecret));
});
test("finalized migration with no legacy tables stays on the ordinary release path", (t) => {
  const fx = withFixture(t, "complete_noop");
  const result = runRelease(fx);
  assert.equal(result.status, 0, result.stderr);
  const commands = log(fx);
  assert.ok(hasCommand(commands, "gitlab-vcs-migrate preflight state_table=present"));
  assert.ok(hasCommand(commands, "migrate up"));
  assert.ok(!hasCommand(commands, "pg_dump"));
  for (const operation of ["begin", "migrate", "switch-hooks", "replay", "verify-hooks", "finalize", "restore", "status"]) {
    assert.ok(!hasCommand(commands, `gitlab-vcs-migrate ${operation}`), `${operation} must not run on the finalized no-op path`);
  }
  assert.ok(hasCommand(commands, "systemctl start multica-backend multica-frontend"));
  assert.ok(hasCommand(commands, "/auth/send-code"));
});
test("no-op data cannot bypass an existing maintenance gate without its recovery record", (t) => {
  const fx = withFixture(t, "maintenance_empty");
  const result = runRelease(fx);
  assert.notEqual(result.status, 0);
  const commands = log(fx);
  assert.ok(hasCommand(commands, "gitlab-vcs-migrate preflight state_table=absent"));
  assert.ok(!hasCommand(commands, "systemctl stop"));
  assert.ok(!hasCommand(commands, "migrate up"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate status"));
  assert.ok(!hasCommand(commands, "systemctl start"));
  assert.match(`${result.stdout}\n${result.stderr}`, /no legacy rows while migration state is maintenance/i);
});
test("Redis-backed legacy cutover fails closed before stopping services", (t) => {
  const fx = withFixture(t, "redis_legacy");
  const result = runRelease(fx);
  assert.notEqual(result.status, 0);
  const commands = log(fx);
  assert.ok(hasCommand(commands, "psql "), "the legacy-row probe is read-only and runs before service stop");
  assert.ok(!hasCommand(commands, "systemctl stop"));
  assert.ok(!hasCommand(commands, "migrate up"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate begin"));
  assert.match(`${result.stdout}\n${result.stderr}`, /REDIS_URL|drain|queue/i);
  assert.ok(hasCommand(commands, "psql libpq configuration secure"));
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(databaseSecret));
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(redisSecret));
  assert.deepEqual(readdirSync(path.join(fx.state, "tmp")), []);
});

test("Redis-backed release fails closed on a partial legacy schema", (t) => {
  const fx = withFixture(t, "redis_partial");
  const result = runRelease(fx);
  assert.notEqual(result.status, 0);
  const commands = log(fx);
  assert.ok(hasCommand(commands, "psql -XAtq -v ON_ERROR_STOP=1"));
  assert.ok(!hasCommand(commands, "systemctl stop"));
  assert.ok(!hasCommand(commands, "migrate up"));
  assert.ok(hasCommand(commands, "psql libpq configuration secure"));
  assert.match(`${result.stdout}\n${result.stderr}`, /partially present/i);
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(databaseSecret));
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(redisSecret));
  assert.deepEqual(readdirSync(path.join(fx.state, "tmp")), []);
});

test("Redis-backed releases continue when the legacy probe finds no rows", (t) => {
  const fx = withFixture(t, "redis_empty");
  const result = runRelease(fx);
  assert.equal(result.status, 0, result.stderr);
  const commands = log(fx);
  assert.ok(hasCommand(commands, "psql -XAtq -v ON_ERROR_STOP=1"));
  assert.ok(hasCommand(commands, "migrate up"));
  assertBefore(commands, "gitlab-vcs-migrate preflight state_table=absent", "migrate up");
  assertBefore(commands, "gitlab-vcs-migrate preflight state_table=absent", "systemctl stop multica-frontend multica-backend");
  assertBefore(commands, "systemctl stop multica-frontend multica-backend", "migrate up");
  assert.ok(hasCommand(commands, "systemctl start multica-backend multica-frontend"));
  assert.ok(!hasCommand(commands, "pg_dump"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate begin"));
  assert.ok(hasCommand(commands, "psql libpq configuration secure"));
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(databaseSecret));
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(redisSecret));
  assert.deepEqual(readdirSync(path.join(fx.state, "tmp")), []);
});
test("interrupted restore resumes with its pinned tool before any new release work", (t) => {
  const fx = withFixture(t, "restore_retry");
  const result = runRelease(fx);
  assert.notEqual(result.status, 0, "a recovery-only invocation must not report a new release as deployed");
  const commands = log(fx);
  assert.ok(hasCommand(commands, "gitlab-vcs-migrate status"));
  assert.ok(hasCommand(commands, "gitlab-vcs-migrate restore"));
  assert.ok(!hasCommand(commands, "pnpm cwd=" + fx.remote + " install --frozen-lockfile"));
  assert.ok(!hasCommand(commands, "migrate up"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate preflight"));
  assert.ok(hasCommand(commands, "systemctl stop multica-frontend multica-backend"));
  assert.ok(hasCommand(commands, "systemctl start multica-backend"));
  assert.equal(readFileSync(path.join(fx.remote, "apps", "web", ".next", "BUILD_ID"), "utf8"), "old-build\n");
  assert.equal(readFileSync(path.join(fx.remote, "server", "bin", "server"), "utf8"), "old server\n");
  assert.equal(readFileSync(path.join(fx.remote, ".gitlab-vcs-cutover.state"), "utf8").includes("status=restored"), true);
  assert.match(`${result.stdout}\n${result.stderr}`, /redeliver|503/i);
});

test("a new HEAD cannot bypass unfinished old-service recovery", (t) => {
  const fx = withFixture(t, "new_commit_restore_active");
  const result = runRelease(fx);
  assert.notEqual(result.status, 0);
  const commands = log(fx);
  assert.ok(hasCommand(commands, "gitlab-vcs-migrate status"));
  assert.ok(!hasCommand(commands, "gitlab-vcs-migrate restore"));
  assert.ok(!hasCommand(commands, "git cwd=" + fx.remote + " fetch"));
  assert.ok(!hasCommand(commands, "pnpm cwd=" + fx.remote + " install --frozen-lockfile"));
  assert.ok(!hasCommand(commands, "systemctl stop"));
  assert.match(`${result.stdout}\n${result.stderr}`, /Rerun its exact HEAD/i);
});

test("a verified restored snapshot permits a new HEAD and retains the old recovery materials", (t) => {
  const fx = withFixture(t, "restored_new_head");
  const oldSnapshot = path.join(fx.remote, fx.env.MULTICA_TEST_RESTORED_DIR);
  const oldDatabase = readFileSync(path.join(oldSnapshot, "database.dump"), "utf8");
  const oldServer = readFileSync(path.join(oldSnapshot, "artifacts", "server.previous"), "utf8");
  const result = runRelease(fx);
  assert.equal(result.status, 0, result.stderr);

  const commands = log(fx);
  assertBefore(commands, "gitlab-vcs-migrate status", `git cwd=${fx.remote} fetch`);
  assert.ok(hasCommand(commands, `git cwd=${fx.remote} merge --ff-only origin/${fx.env.MULTICA_TEST_BRANCH}`));
  assert.equal(readFileSync(path.join(oldSnapshot, "database.dump"), "utf8"), oldDatabase);
  assert.equal(readFileSync(path.join(oldSnapshot, "artifacts", "server.previous"), "utf8"), oldServer);
  assert.ok(existsSync(oldSnapshot), "the restored generation remains available after forward release");
  assert.ok(existsSync(path.join(fx.remote, ".gitlab-vcs-cutover-feedface-2", "database.dump")));
});

test("a similar unrecorded cutover directory remains an unexpected remote change", (t) => {
  const fx = withFixture(t, "restored_unknown_dir");
  const oldSnapshot = path.join(fx.remote, fx.env.MULTICA_TEST_RESTORED_DIR);
  const unknownFile = path.join(fx.remote, fx.env.MULTICA_TEST_UNKNOWN_CUTOVER_DIR, "unrelated.txt");
  const oldDatabase = readFileSync(path.join(oldSnapshot, "database.dump"), "utf8");
  const result = runRelease(fx);
  assert.notEqual(result.status, 0);

  const commands = log(fx);
  assert.ok(hasCommand(commands, "git cwd=" + fx.remote + " status --porcelain"));
  assert.ok(!hasCommand(commands, `git cwd=${fx.remote} merge --ff-only origin/${fx.env.MULTICA_TEST_BRANCH}`));
  assert.ok(!hasCommand(commands, "systemctl stop"));
  assert.match(`${result.stdout}\n${result.stderr}`, /unexpected local changes/i);
  assert.equal(readFileSync(path.join(oldSnapshot, "database.dump"), "utf8"), oldDatabase);
  assert.equal(readFileSync(unknownFile, "utf8"), "preserve me");
});
test("one-time exact-instance System Hook attestation is passed and pinned for cutover retries", (t) => {
  const fx = withFixture(t, "success");
  fx.env.MULTICA_GITLAB_SYSTEM_HOOKS_ATTESTED = "https://gitlab.example.test";
  const result = runRelease(fx);
  assert.equal(result.status, 0, result.stderr);

  const commands = log(fx);
  const option = "--attest-no-legacy-system-hooks-for https://gitlab.example.test";
  assert.ok(hasCommand(commands, `gitlab-vcs-migrate preflight ${option}`));
  assert.ok(hasCommand(commands, `gitlab-vcs-migrate begin ${option}`));
  assert.ok(commands.some((command) => command.includes("MULTICA_RELEASE_SYSTEM_HOOK_ATTESTATION=https://gitlab.example.test")));

  const cutoverState = readFileSync(path.join(fx.remote, ".gitlab-vcs-cutover.state"), "utf8");
  assert.match(cutoverState, /system_hook_attestation=https:\/\/gitlab\.example\.test/);
  assert.equal(statSync(path.join(fx.remote, ".gitlab-vcs-cutover.state")).mode & 0o777, 0o600);
  assert.ok(!readFileSync(path.join(fx.remote, ".env"), "utf8").includes("MULTICA_GITLAB_SYSTEM_HOOKS_ATTESTED"));
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(databaseSecret));
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(redisSecret));
});

test("release rejects a System Hook attestation with a path before remote work", (t) => {
  const fx = withFixture(t, "success");
  fx.env.MULTICA_GITLAB_SYSTEM_HOOKS_ATTESTED = "https://gitlab.example.test/group";
  const result = runRelease(fx);
  assert.notEqual(result.status, 0);
  const commands = log(fx);
  assert.ok(!hasCommand(commands, "ssh "));
  assert.ok(!hasCommand(commands, "migrate up"));
  assert.match(`${result.stdout}\n${result.stderr}`, /complete HTTPS origin/i);
});
test("preparing cutover retry reuses its pinned System Hook attestation", (t) => {
  const fx = withFixture(t, "resume_preparing_attested");
  const result = runRelease(fx);
  assert.equal(result.status, 0, result.stderr);

  const commands = log(fx);
  const option = "--attest-no-legacy-system-hooks-for https://gitlab.example.test";
  assert.ok(hasCommand(commands, `gitlab-vcs-migrate preflight ${option}`));
  assert.ok(hasCommand(commands, `gitlab-vcs-migrate begin ${option}`));
  assert.ok(hasCommand(commands, "gitlab-vcs-migrate switch-hooks"));
  const state = readFileSync(path.join(fx.remote, ".gitlab-vcs-cutover.state"), "utf8");
  assert.match(state, /status=complete/);
  assert.match(state, /system_hook_attestation=https:\/\/gitlab\.example\.test/);
  assert.ok(!`${result.stdout}\n${result.stderr}\n${commands.join("\n")}`.includes(databaseSecret));
});

test("a full ancestor upstream pin skips upstream updates and preserves origin checks and normal push", (t) => {
  const fx = withFixture(t, "pin_success");
  const releaseHead = "b".repeat(40);
  fx.env.MULTICA_UPSTREAM_PIN = upstreamPin;
  fx.env.MULTICA_TEST_HEAD = releaseHead;
  const result = runRelease(fx);
  assert.equal(result.status, 0, result.stderr);

  const commands = log(fx);
  assert.ok(hasCommand(commands, `rev-parse --verify ${upstreamPin}^{commit}`));
  assert.ok(hasCommand(commands, `merge-base --is-ancestor ${upstreamPin} HEAD`));
  assert.ok(!hasCommand(commands, "fetch upstream main --tags"));
  assert.ok(!hasCommand(commands, "merge --no-ff upstream/main"));
  assert.ok(!hasCommand(commands, "rebase upstream/main"));
  assert.ok(hasCommand(commands, "ls-remote --exit-code --heads origin feature/gitlab-cutover"));
  assert.ok(hasCommand(commands, "fetch origin feature/gitlab-cutover"));
  assert.ok(hasCommand(commands, "rev-list --count HEAD..origin/feature/gitlab-cutover"));
  assert.ok(hasCommand(commands, "push origin feature/gitlab-cutover"));
  assert.ok(!hasCommand(commands, "push --force"));
  assert.ok(commands.some((command) => command.includes(`EXPECTED_HEAD=${releaseHead}`)));
  assert.match(result.stdout, new RegExp(`pinned baseline ${upstreamPin}`));
});

test("upstream pins reject malformed or unresolved full commit identifiers", (t) => {
  const malformed = withFixture(t, "pin_invalid_format");
  malformed.env.MULTICA_UPSTREAM_PIN = "abc1234";
  const malformedResult = runRelease(malformed);
  assert.notEqual(malformedResult.status, 0);
  assert.match(`${malformedResult.stdout}\n${malformedResult.stderr}`, /full 40- or 64-character commit SHA/i);
  assert.ok(!hasCommand(log(malformed), "rev-parse --verify"));
  assert.ok(!hasCommand(log(malformed), "ls-remote --exit-code --heads origin"));

  const missing = withFixture(t, "pin_missing_commit");
  missing.env.MULTICA_UPSTREAM_PIN = upstreamPin;
  const missingResult = runRelease(missing);
  assert.notEqual(missingResult.status, 0);
  assert.ok(hasCommand(log(missing), `rev-parse --verify ${upstreamPin}^{commit}`));
  assert.match(`${missingResult.stdout}\n${missingResult.stderr}`, /does not resolve to an existing commit/i);
  assert.ok(!hasCommand(log(missing), "ls-remote --exit-code --heads origin"));
});

test("an upstream pin outside HEAD is rejected before origin or release work", (t) => {
  const fx = withFixture(t, "pin_not_ancestor");
  fx.env.MULTICA_UPSTREAM_PIN = upstreamPin;
  const result = runRelease(fx);
  assert.notEqual(result.status, 0);
  const commands = log(fx);
  assert.ok(hasCommand(commands, `merge-base --is-ancestor ${upstreamPin} HEAD`));
  assert.match(`${result.stdout}\n${result.stderr}`, /is not an ancestor of HEAD/i);
  assert.ok(!hasCommand(commands, "fetch upstream main --tags"));
  assert.ok(!hasCommand(commands, "ls-remote --exit-code --heads origin"));
  assert.ok(!hasCommand(commands, "push origin feature/gitlab-cutover"));
});

test("an upstream pin still refuses to push when origin contains newer commits", (t) => {
  const fx = withFixture(t, "pin_origin_ahead");
  fx.env.MULTICA_UPSTREAM_PIN = upstreamPin;
  const result = runRelease(fx);
  assert.notEqual(result.status, 0);
  const commands = log(fx);
  assert.ok(hasCommand(commands, `merge-base --is-ancestor ${upstreamPin} HEAD`));
  assert.ok(hasCommand(commands, "ls-remote --exit-code --heads origin feature/gitlab-cutover"));
  assert.ok(hasCommand(commands, "fetch origin feature/gitlab-cutover"));
  assert.ok(hasCommand(commands, "rev-list --count HEAD..origin/feature/gitlab-cutover"));
  assert.ok(!hasCommand(commands, "fetch upstream main --tags"));
  assert.ok(!hasCommand(commands, "push origin feature/gitlab-cutover"));
  assert.match(`${result.stdout}\n${result.stderr}`, /origin\/feature\/gitlab-cutover contains 1 commit.*not present locally/i);
});
const realPgBinDir = process.env.MULTICA_TEST_PG_BIN_DIR || "";
const realPgTools = ["initdb", "pg_ctl", "psql", "pg_dump", "pg_restore"].map((name) => [name, path.join(realPgBinDir, name)]);
const realPgEnabled = Boolean(
  realPgBinDir
  && process.getuid?.() !== 0
  && realPgTools.every(([, executable]) => existsSync(executable)),
);

function runPgTool(executable, args, { input, env: extraEnv = {} } = {}) {
  const env = { ...process.env };
  for (const name of Object.keys(env)) {
    if (name.startsWith("PG")) delete env[name];
  }
  Object.assign(env, extraEnv);
  const result = spawnSync(executable, args, {
    encoding: "utf8",
    env,
    input,
    maxBuffer: 8 * 1024 * 1024,
    timeout: 120_000,
  });
  if (result.error || result.status !== 0) {
    throw new Error(`${path.basename(executable)} test helper failed`);
  }
  return result.stdout;
}

async function reservePort() {
  const server = createServer();
  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const address = server.address();
  await new Promise((resolve, reject) => server.close((error) => error ? reject(error) : resolve()));
  return address.port;
}

function makeRestoreService(directory, { port, database, password }) {
  const serviceFile = path.join(directory, "restore.pg_service.conf");
  const passFile = path.join(directory, "restore.pgpass");
  const escaped = password.replace(/\\/g, "\\\\").replace(/:/g, "\\:");
  writeFileSync(passFile, `127.0.0.1:${port}:${database}:release_user:${escaped}\n`, { mode: 0o600 });
  chmodSync(passFile, 0o600);
  writeFileSync(serviceFile, [
    "[multica_restore]",
    "host=127.0.0.1",
    `port=${port}`,
    `dbname=${database}`,
    "user=release_user",
    "sslmode=require",
    "application_name=restore integration",
    "connect_timeout=3",
    `passfile=${passFile}`,
    "",
  ].join("\n"), { mode: 0o600 });
  chmodSync(serviceFile, 0o600);
  return { PGSERVICEFILE: serviceFile, PGSERVICE: "multica_restore" };
}

test("PostgreSQL URI backup and Redis probe use the workspace libpq connection", {
  skip: !realPgEnabled && "set MULTICA_TEST_PG_BIN_DIR to a non-root local PostgreSQL installation",
  timeout: 120_000,
}, async (t) => {
  const tempRoot = mkdtempSync(path.join(os.tmpdir(), "multica-pg-real-"));
  const dataDir = path.join(tempRoot, "data");
  const logFile = path.join(tempRoot, "postgres.log");
  const pg = Object.fromEntries(realPgTools);
  const openssl = process.env.MULTICA_TEST_OPENSSL_BIN || "openssl";
  let serverStarted = false;
  t.after(() => {
    let stopFailed = false;
    if (serverStarted) {
      const stopped = spawnSync(pg.pg_ctl, ["-D", dataDir, "-m", "fast", "-w", "-t", "30", "stop"], {
        encoding: "utf8",
        timeout: 45_000,
      });
      stopFailed = Boolean(stopped.error || stopped.status !== 0);
    }
    rmSync(tempRoot, { recursive: true, force: true });
    if (stopFailed) throw new Error("temporary PostgreSQL server did not stop");
  });

  mkdirSync(dataDir, { recursive: true, mode: 0o700 });
  const initPasswordFile = path.join(tempRoot, "initdb-password");
  writeFileSync(initPasswordFile, "temporary-superuser-password\n", { mode: 0o600 });
  chmodSync(initPasswordFile, 0o600);
  runPgTool(pg.initdb, ["-D", dataDir, "--auth-local=trust", "--auth-host=scram-sha-256", "--username=postgres", "--pwfile", initPasswordFile]);

  const cert = path.join(dataDir, "server.crt");
  const key = path.join(dataDir, "server.key");
  runPgTool(openssl, [
    "req", "-x509", "-newkey", "rsa:2048", "-nodes", "-days", "1",
    "-subj", "/CN=localhost", "-keyout", key, "-out", cert,
  ]);
  chmodSync(key, 0o600);
  const configString = (value) => `'${value.replace(/\\/g, "\\\\").replace(/'/g, "\\'")}'`;
  writeFileSync(path.join(dataDir, "postgresql.auto.conf"), [
    "ssl=on",
    `ssl_cert_file=${configString("server.crt")}`,
    `ssl_key_file=${configString("server.key")}`,
    `unix_socket_directories=${configString(os.tmpdir())}`,
    "",
  ].join("\n"), { mode: 0o600 });

  const port = await reservePort();
  serverStarted = true;
  runPgTool(pg.pg_ctl, [
    "-D", dataDir,
    "-l", logFile,
    "-o", `-h 127.0.0.1 -p ${port}`,
    "-w", "-t", "30", "start",
  ]);

  const password = "p@ss:/+%:colon\\slash";
  const sqlLiteral = password.replace(/'/g, "''");
  const setupSql = [
    `CREATE ROLE release_user LOGIN PASSWORD '${sqlLiteral}';`,
    "CREATE DATABASE release_backup OWNER release_user;",
    "CREATE DATABASE release_probe OWNER release_user;",
    "CREATE DATABASE release_restore OWNER release_user;",
    "",
  ].join("\n");
  const localPsql = (user, database, input) => runPgTool(pg.psql, [
    "-X", "-h", os.tmpdir(), "-p", String(port), "-U", user, "-d", database, "-v", "ON_ERROR_STOP=1",
  ], { input });
  localPsql("postgres", "postgres", setupSql);
  localPsql("release_user", "release_backup", "CREATE TABLE release_sample (id integer PRIMARY KEY, payload text NOT NULL);\nINSERT INTO release_sample VALUES (42, 'restored from the custom archive');\n");
  localPsql("release_user", "release_probe", [
    "CREATE TABLE gitlab_connection (id integer);",
    "INSERT INTO gitlab_connection VALUES (1);",
    "CREATE TABLE gitlab_project_binding (id integer);",
    "CREATE TABLE gitlab_merge_request (id integer);",
    "CREATE TABLE issue_gitlab_merge_request (id integer);",
    "CREATE TABLE gitlab_mr_pipeline (id integer);",
    "",
  ].join("\n"));

  const appName = "release integration";
  const uriFor = (database) => `postgresql://release_user:${encodeURIComponent(password)}@127.0.0.1:${port}/${database}?sslmode=require&application_name=${encodeURIComponent(appName)}&connect_timeout=3`;
  const configureRealFixture = (fx, database) => {
    fx.env.MULTICA_REAL_PG = "1";
    fx.env.MULTICA_REAL_PG_DUMP = pg.pg_dump;
    fx.env.MULTICA_REAL_PSQL = pg.psql;
    fx.env.MULTICA_TEST_DB_PASSWORD = password;
    fx.env.MULTICA_TEST_DBHOST = "127.0.0.1";
    fx.env.MULTICA_TEST_DBPORT = String(port);
    fx.env.MULTICA_TEST_DBUSER = "release_user";
    fx.env.MULTICA_TEST_DBNAME = database;
    fx.env.MULTICA_TEST_SSLMODE = "require";
    fx.env.MULTICA_TEST_APPNAME = appName;
    fx.env.MULTICA_TEST_CONNECT_TIMEOUT = "3";
  };

  const backupUrl = uriFor("release_backup");
  const backupFixture = withFixture(t, "real_pg_backup", backupUrl);
  assert.equal(statSync(path.join(backupFixture.remote, ".env")).mode & 0o777, 0o600);
  configureRealFixture(backupFixture, "release_backup");
  const backupRelease = runRelease(backupFixture);
  assert.equal(backupRelease.status, 0, "the release fixture should complete its mocked cutover without a real remote");
  const backupCommands = log(backupFixture);
  assert.ok(hasCommand(backupCommands, "pg_dump libpq configuration secure"));
  assert.deepEqual(readdirSync(path.join(backupFixture.state, "tmp")), []);
  const backupPaths = [];
  const visitBackups = (directory) => {
    for (const entry of readdirSync(directory, { withFileTypes: true })) {
      const full = path.join(directory, entry.name);
      if (entry.isDirectory()) visitBackups(full);
      else if (entry.name.endsWith(".dump")) backupPaths.push(full);
    }
  };
  visitBackups(backupFixture.remote);
  assert.equal(backupPaths.length, 1);
  const backupPath = backupPaths[0];
  assert.equal(statSync(backupPath).mode & 0o777, 0o600);
  runPgTool(pg.pg_restore, ["--list", backupPath]);

  const restoreEnv = makeRestoreService(path.join(backupFixture.state, "tmp"), {
    port,
    database: "release_restore",
    password,
  });
  assert.equal(statSync(restoreEnv.PGSERVICEFILE).mode & 0o777, 0o600);
  const restorePassfile = readFileSync(restoreEnv.PGSERVICEFILE, "utf8").match(/^passfile=(.+)$/m)?.[1];
  assert.ok(restorePassfile);
  assert.equal(statSync(restorePassfile).mode & 0o777, 0o600);
  runPgTool(pg.pg_restore, [
    "--clean", "--if-exists", "--no-owner", "--no-acl",
    "--dbname", "service=multica_restore",
    backupPath,
  ], { env: restoreEnv });
  const restored = runPgTool(pg.psql, [
    "-XAtq", "-v", "ON_ERROR_STOP=1", "-c", "SELECT payload FROM release_sample WHERE id = 42",
  ], { env: restoreEnv });
  assert.equal(restored.trim(), "restored from the custom archive");

  const probeUrl = uriFor("release_probe");
  const probeFixture = withFixture(t, "real_pg_probe", probeUrl);
  configureRealFixture(probeFixture, "release_probe");
  const probeRelease = runRelease(probeFixture);
  assert.notEqual(probeRelease.status, 0);
  const probeCommands = log(probeFixture);
  assert.ok(hasCommand(probeCommands, "psql libpq configuration secure"));
  assert.ok(!hasCommand(probeCommands, "systemctl stop"));
  assert.ok(!hasCommand(probeCommands, "migrate up"));
  assert.ok(!hasCommand(probeCommands, "pg_dump"));
  assert.match(`${probeRelease.stdout}\n${probeRelease.stderr}`, /bounded per-process queue|in-process queue/i);
  assert.deepEqual(readdirSync(path.join(probeFixture.state, "tmp")), []);

  for (const [release, commands, url] of [
    [backupRelease, backupCommands, backupUrl],
    [probeRelease, probeCommands, probeUrl],
  ]) {
    const observed = `${release.stdout}\n${release.stderr}\n${commands.join("\n")}`;
    assert.ok(!observed.includes(password));
    assert.ok(!observed.includes(url));
  }
});
