---
name: multica-selfhost-release
description: This skill should be used when the user asks to "deploy Multica self-hosted", "sync upstream/main", "update dj", "run a selfhost release", "package Multica desktop", "deploy Loop sync", or "merge or rebase a selfhost branch". It deploys through direct SSH to dj, manages Loop sync on my-mini, and packages the self-hosted Multica app.
---

# Multica Selfhost Release

Use this skill for the recurring self-hosted Multica release path:

1. Commit only task-related changes on the current branch.
2. Inspect the branch lifecycle and published state, then choose merge, rebase, or no-op for `upstream/main`.
3. Push normally after merge/no-op; use `--force-with-lease` only when rebasing an already-published branch.
4. Build the current branch's `multica` CLI, uninstall Homebrew `multica`, and install the binary to `~/.local/bin/multica` locally and on `my-mini`.
5. Build and typecheck the web app natively on this Mac; package only `apps/web/.next` with macOS metadata disabled.
6. Update `dj:~/apps/multica` through direct local `ssh dj`, install Linux dependencies, run migrations, build Linux Go binaries, and atomically replace the uploaded `.next` artifact.
7. Run local `./scripts/package.sh`.
8. Replace local `/Applications/Multica.app` with the generated app bundle.
9. Verify remote services and local app/CLI version/config.

## Choose Merge or Rebase

Do not mechanically rebase every release. Decide from the actual branch state and the cost of the next upstream update.

| Dimension | Rebase | Merge |
| --- | --- | --- |
| History | Rewrites the branch's unique commits onto the new base | Preserves published ancestry and adds one merge commit |
| Push | Published branches require `--force-with-lease` | Normal push |
| Deployed/shared checkout | May stop fast-forwarding and require a verified reset | Usually continues to fast-forward |
| Conflict handling | Replays commits one by one; the same logical conflict may recur | Resolves the upstream integration once per merge |
| Best fit | Short-lived, private branch with few commits; clean history before an upstream PR | Long-lived self-host branch that is published, deployed, shared, or repeatedly updated from upstream |

Use this decision order:

1. If `upstream/main` is already an ancestor of `HEAD`, do not create a merge commit or rebase; push normally.
2. If `origin/<branch>` contains commits absent locally, stop and integrate those commits first. Never overwrite them during release.
3. Choose **merge** when the branch is long-lived, already published/deployed, has substantial independent history, is used by another checkout, or will keep receiving upstream updates.
4. Choose **rebase** only when the branch is private or disposable, has no downstream consumer, and rewriting its commits is intentional.
5. If evidence is mixed, prefer merge because it preserves history and makes later recurring upstream integrations easier.

The script's `auto` mode uses published state as a conservative mechanical proxy: published branch -> merge; unpublished branch -> rebase. For a known long-lived branch, explicitly set `MULTICA_UPSTREAM_SYNC_STRATEGY=merge` rather than relying only on the proxy.

`dj` is directly reachable from this Mac with `ssh dj`; use that route for the deployment checkout, protected release window, and remote verification. `my-mini` hosts the separately installed and signed CLI and the independent Loop sync service.

## Loop Sync on my-mini

`apps/loop-sync` runs as the user-level Super service `multica-loop-sync`, independently of the `dj` backend and web release. Deploy only when Loop sync is in scope; it does not require the full Multica release workflow.

- Code: `my-mini:~/apps/loop-sync/`.
- Configuration: `my-mini:~/.config/loop-sync/config.json`; each binding has a Loop credential environment-variable name, both workspace IDs, member mappings and its own state file. See `apps/loop-sync/README.md`.
- Credentials: `my-mini:~/.config/loop-sync/tokens.env`, mode 600. Super sources this file without printing its values. Keep credentials and state off the repository; never replace existing config, tokens or state during code deployment.
- State: one file per binding under `~/.config/loop-sync/state/`; retain it across deployments. Confirm the intended mappings before enabling a binding: the first scan imports all matching open Loop tasks.
- Super declaration: `~/.super/conf/conf.d/loop-sync.toml`, `prune = false`, `autostart = true`, command `/Users/dong/.local/bin/node /Users/dong/apps/loop-sync/main.mjs /Users/dong/.config/loop-sync/config.json`.

Run the focused tests and update the service without syncing configuration or state:

```bash
node --test apps/loop-sync/*.test.mjs
rsync -az apps/loop-sync/ my-mini:apps/loop-sync/
ssh my-mini 'export PATH="$HOME/.local/bin:$PATH" SUPER_ROOT="$HOME/.super"; super restart multica-loop-sync --wait-healthy --timeout 30; super info multica-loop-sync; super logs multica-loop-sync --tail 50'
```

If the service is not registered, create the user-level Super declaration with the paths above and apply it with `super apply --file ~/.super/conf/conf.d/loop-sync.toml`. An empty `bindings` array keeps the service running but does not synchronize anything. After editing configuration or credentials, restart the service; check logs and verify actual task synchronization separately from Super's process health.

## Important Judgement

- Treat remote `apps/web/next-env.d.ts` changes as Next build noise unless the user explicitly asks to track generated type path changes.
- Treat `package.json` `pnpm.onlyBuiltDependencies` additions as valuable when `pnpm install` or packaging needs native/postinstall dependencies such as `sharp`, `electron-winstaller`, `protobufjs`, `msw`, `core-js`, `unicode-animations`, or `unrs-resolver`.
- Do not use `git add .`. Stage only the files that belong to the requested release.
- If the remote working tree has a valuable change, copy it back locally, commit it on the current branch, apply the chosen upstream synchronization strategy, push, then update the remote checkout.
- If the remote working tree has only generated noise, leave it uncommitted or restore it only when it blocks a Git operation.
- If a rebase intentionally rewrites published history, the deployment checkout may no longer fast-forward. The script must stop rather than reset automatically; inspect the remote commits and only perform a manual reset after separately confirming the rewrite and target SHA.
- Never use a force push after merge. Never use plain `--force`; a published rebase requires `--force-with-lease`.
- Pin one final `HEAD` after synchronization. Build the CLI, update the deployment checkout, package the desktop app, and verify all artifacts against that commit. If conflict repair or any other fix creates a later commit, restart the full workflow from CLI build; never mix artifacts from different commits.
- On `my-mini`, ad-hoc sign the uploaded CLI before replacing `~/.local/bin/multica`, then run `codesign --verify --strict` and `multica version`.

## Native OMP Merge Policy (2026-08-11)

The native OMP merge was integrated in `4b009c11b8c4b707e4d7e2cd898e53a3b2d09255`. Treat this as a behavioral boundary, not a provider-label rename:

- The OMP descriptor may share only runtime metadata and the minimal JSONL event decoder with Pi. OMP remains an independent runtime identity, profile, discovery path, and display name.
- Invoke OMP as `omp -p --mode json --session-dir <dir> [--resume <id>] [--model <provider/model>] [--thinking <level>] <prompt>`. Do not replace it with Pi's `--session <file>` protocol or split the complete selector into Pi's provider/model flags.
- Discover with `omp models --no-extensions --json` and a 15-second timeout. Retain `MULTICA_OMP_MODEL` as the full-selector manual fallback when discovery fails.
- Classify explicit missing-session failures from bounded stderr as resume rejection; only then retry once from a guarded fresh session.
- Catalog data governs per-model thinking compatibility. Do not hard-code Pi's or a provider-wide OMP effort allowlist.
- Keep workspace-injected Skills in `.omp/skills/<slug>/SKILL.md` and OMP user Skills in `.omp/agent/skills`; never write OMP Skills under `.pi`.
- Use uppercase `OMP` in runtime labels, documentation, UI aliases, and transcript rendering.

When resolving future `upstream/main` conflicts, retain these native semantics even if upstream changes the Pi-compatible adapter. A post-merge repair creates a new final HEAD, so rebuild every release artifact from that commit.

## Local Web Artifact Deployment

Build the web app natively on this Mac. This is the default because the verified native build completes in about one minute, while Webpack on the constrained self-host and Linux/amd64 emulation on Apple Silicon can stall or consume more than 4 GiB.

- Run `pnpm --filter @multica/web typecheck`, then build with `MULTICA_WEB_TYPECHECK_ALREADY_PASSED=1`.
- Restore generated `apps/web/next-env.d.ts` immediately after the build so the pinned release version never gains a false `-dirty` suffix.
- Package only `apps/web/.next` with `COPYFILE_DISABLE=1`; never upload local `node_modules` or a Darwin-native standalone bundle.
- Require non-empty `BUILD_ID`, `routes-manifest.json`, and `prerender-manifest.json` before upload and after extraction.
- Keep `pnpm install --frozen-lockfile` on `dj`; the running Next service needs Linux/x86_64 native dependencies from the remote `node_modules`.
- Build backend, CLI, and migration binaries on `dj` so they target Linux/x86_64.
- Stage web and Go artifacts before stopping services. During the short release window, run migrations, preserve the previous `.next` and binaries, replace them, restart both services, and roll back artifacts if any later step fails.
- Bind the archive name and every verification to the pinned final commit. A later commit requires rebuilding all artifacts.

Do not substitute Bun or a `linux/amd64` Docker build on this Apple Silicon host. Bun does not replace Next/Webpack, and QEMU makes this CPU-heavy build slower. The checked-in `Dockerfile.web` remains for image deployments, not this systemd self-host path.

## Standard Workflow

Before running the full workflow:

```bash
cd /Users/dong/.wtc/projects/multica
git status --short --branch
git log --oneline --decorate -5
```

If there are uncommitted task changes, run targeted tests, stage only those files, then commit. Inspect both upstream and origin before choosing a strategy:

```bash
branch="$(git rev-parse --abbrev-ref HEAD)"
git fetch upstream main --tags
git rev-list --left-right --count "upstream/main...HEAD"
git merge-base --is-ancestor upstream/main HEAD && echo "upstream already integrated"
if git ls-remote --exit-code --heads origin "$branch" >/dev/null; then
  git fetch origin "$branch"
  git rev-list --left-right --count "HEAD...origin/$branch"
else
  remote_status=$?
  if [ "$remote_status" -ne 2 ]; then
    echo "Cannot determine origin branch state (exit $remote_status); abort." >&2
    exit 1
  fi
fi
```

For a long-lived or published self-host branch, merge and push normally:

```bash
git merge --no-ff upstream/main -m "chore: merge upstream/main"
git push origin "$branch"
```

For a private short-lived branch, rebase. Force-with-lease is only for an intentionally rewritten published branch:

```bash
git rebase upstream/main
git push origin "$branch"                         # unpublished branch
git push --force-with-lease origin "$branch"      # published branch, rewrite explicitly intended
```

Build and install the current CLI on this Mac and on `my-mini`:

```bash
cd /Users/dong/.wtc/projects/multica
(cd server && go build -ldflags "-X main.version=$(git describe --tags --always --dirty) -X main.commit=$(git rev-parse --short HEAD) -X main.date=$(date -u '+%Y-%m-%dT%H:%M:%SZ')" -o bin/multica ./cmd/multica)

brew list --formula multica >/dev/null 2>&1 && brew uninstall multica || true
mkdir -p ~/.local/bin
install -m 0755 server/bin/multica ~/.local/bin/multica
~/.local/bin/multica version

ssh my-mini 'zsh -lc "mkdir -p ~/.local/bin && if command -v brew >/dev/null 2>&1 && brew list --formula multica >/dev/null 2>&1; then brew uninstall multica; fi"'
scp server/bin/multica my-mini:~/.local/bin/multica.upload
ssh my-mini 'zsh -lc "chmod 0755 ~/.local/bin/multica.upload && codesign --force --sign - ~/.local/bin/multica.upload && mv ~/.local/bin/multica.upload ~/.local/bin/multica && codesign --verify --strict ~/.local/bin/multica && ~/.local/bin/multica version"'
```

Update and deploy remote through direct local `ssh dj`; use `run_release.sh` for the exact implementation rather than copying a partial inline command. The remote CLI preflight is read-only and must run against the existing schema before service stop, backup, or any application DDL. `status` remains strict and is only called after successful `migrate up` has created its state table. A no-op report is accepted only for `complete` or `not_initialized`; a contradictory unfinished maintenance state fails closed before stopping services. Accepted no-op reports skip GitLab backup/cutover work and preserve the ordinary stop/migrate/replace/restart/verify path. New releases build `gitlab-vcs-migrate` from the remote checkout after verifying the final pinned `HEAD`. A durable `preparing` record lets the exact same HEAD resume after interruption before the state table exists; a failed migration leaves services stopped and never probes strict status. Restoring/restore-blocked cutovers and restored old-service recovery are handled first with the saved pinned tool.

### Legacy GitLab Cutover During Release

When preflight finds legacy GitLab data, the script stops both services, saves the old app artifacts, writes a mode-600 PostgreSQL custom backup from the remote checkout's own `.env`, pins the recovery tool and lower time bound, and durably records the preparing phase before running `migrate up`. No application DDL is run before the read-only preflight and database backup. Only after successful `migrate up` may the script call strict `status`; then it begins the maintenance-gated import. Credentials are never printed or passed as command-line arguments. A missing/unreadable environment, backup, tool, or status fails closed; a failed schema migration leaves the old services stopped.

The gated sequence is `begin` → `migrate` → install/start the new app while maintenance remains enabled → `switch-hooks` → `replay --since <saved UTC time>` → `verify-hooks --since <same time>` → `finalize`. Only verified Hooks, real persisted/reconciled receipts, and a confirmed `complete`/`maintenance_mode=false` status permit normal writes. The release captures status-reported 503 delivery IDs in the pinned recovery directory without payloads. GitLab 13.12.15 does not provide the project Hook test-trigger or event-delivery history APIs; the release neither fabricates events nor claims an API-backed inventory of failed deliveries. After Finalize or restore, reconcile listed 503 IDs using version-supported evidence and redeliver; explicitly mark any unenumerable/unrecoverable attempts as unverified. Never claim complete receipt recovery when source-side history is unavailable.

Manual Hook configuration or missing real deliveries leaves the new app running behind the maintenance gate and exits as pending; configure each Hook to the exact workspace-specific URL emitted by the tool, then rerun from the same pinned `HEAD`. GitLab 13.12.15 requires genuine project activity for the required MR, pipeline, job, and note receipts; prepare operators and safe event actions before the window, then perform genuine activity in every affected project after Hook switch while the endpoint remains gated. These operations are not automated by the release script. The release neither fabricates events nor claims an API-backed inventory of failed deliveries: 13.12.15 lacks project Hook test-trigger and event-delivery history APIs, so only source evidence available for that version can establish which 503 deliveries to redeliver; otherwise their history is unverified and cannot be silently ignored. If the events or delivery evidence cannot be obtained, expect prolonged downtime: keep the release gated/restore safely and report the gap rather than skip verification or claim completeness. Do not advance the branch during an incomplete cutover. On other pre-finalization failures, the script calls `restore`; it restores old artifacts and starts only the pre-release services if the CLI confirms `phase=restored` (including Hook and database snapshot restoration). Otherwise it leaves writers stopped/maintenance-gated and prints recovery instructions and known delivery IDs. After old-service restoration, inspect available GitLab delivery evidence, redeliver the listed 503 deliveries, and explicitly acknowledge unrecoverable IDs. After `finalize`, it never rolls back old binaries or a database backup; investigate forward.

If the workspace `.env` defines `REDIS_URL`, the script runs a read-only probe using `to_regclass` and checks only legacy tables that exist, so finalized deployments with all old tables absent can continue normally. An empty full legacy schema permits the ordinary release path; a partial schema or indeterminate query fails closed for inspection. If legacy GitLab rows exist, release stops before service/migration/Hook changes: `REDIS_URL` enables WeCom RelayOutbound's bounded **in-process** queue, and this binary has no per-replica ingress-quiesce/drain acknowledgement. Neither `systemctl stop` nor inspecting Redis queues proves that the in-process queue drained. There is no supported manual command to unblock this release; first deploy a compatible relay with a verifiable per-replica drain acknowledgement and update release validation to verify every replica. Do not attempt this cutover with the old relay. Deployments with no legacy rows retain the ordinary release path; the currently inspected `dj` environment has no `REDIS_URL`.

Build and install locally:

```bash
cd /Users/dong/.wtc/projects/multica
./scripts/package.sh
osascript -e 'tell application "Multica" to quit' || true
rm -rf /Applications/Multica.app
ditto apps/desktop/dist/mac-arm64/Multica.app /Applications/Multica.app
xattr -dr com.apple.quarantine /Applications/Multica.app || true
open -a /Applications/Multica.app
```

Verify:

```bash
/usr/libexec/PlistBuddy -c 'Print :CFBundleShortVersionString' /Applications/Multica.app/Contents/Info.plist
~/.local/bin/multica version
ssh my-mini 'zsh -lc "~/.local/bin/multica version"'
cat ~/.multica/desktop.json
ssh dj 'cd ~/apps/multica && git status --short --branch && git rev-parse HEAD && systemctl is-active multica-backend multica-frontend'
```

Verify that the local app version, local CLI commit, `my-mini` CLI commit, remote checkout SHA, and expected final `HEAD` all match. A mismatch means the release is incomplete; rebuild and reinstall from the final commit.

## Scripted Path

For the routine case where the local worktree is already clean and all changes are committed, run:

```bash
/Users/dong/.wtc/projects/multica/.agents/skills/multica-selfhost-release/scripts/run_release.sh
```

Useful environment overrides:

```bash
MULTICA_REPO=/Users/dong/.wtc/projects/multica
MULTICA_UPSTREAM_SYNC_STRATEGY=auto  # auto | merge | rebase
MULTICA_MY_MINI_HOST=my-mini       # CLI installation host only
MULTICA_REMOTE_HOST=dj             # direct deployment host
MULTICA_REMOTE_DIR=/home/ubuntu/apps/multica
MULTICA_REMOTE_NAME=wingeddragon
MULTICA_WEB_BUILD_MAX_OLD_SPACE_SIZE_MB=4096
MULTICA_SKIP_DEPLOY=1
MULTICA_SKIP_PACKAGE=1
MULTICA_SKIP_INSTALL=1
MULTICA_SKIP_CLI_INSTALL=1
```

The script intentionally exits on a dirty local worktree, an invalid strategy, an indeterminate origin lookup, an origin branch with commits missing locally, or a non-fast-forward deployment checkout. Commit, integrate, or inspect deliberately first, then rerun it.

## Personal Android APK

For a locally built, self-signed Android release APK that targets the deployed
self-host rather than the public Multica endpoints, read
[`references/android-personal-apk.md`](references/android-personal-apk.md).
This is a client-only workflow; it neither deploys nor changes the self-host.
