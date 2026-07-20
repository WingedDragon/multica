---
name: multica-selfhost-release
description: Deploy and package the self-hosted Multica app, including choosing merge or rebase for upstream synchronization. Use when the user asks to update or upload the current Multica branch to dj:~/apps/multica, sync upstream/main, run scripts/deploy.sh through my-mini, build the macOS DMG with scripts/package.sh, replace /Applications/Multica.app, or repeat the full self-hosted release/install workflow.
---

# Multica Selfhost Release

Use this skill for the recurring self-hosted Multica release path:

1. Commit only task-related changes on the current branch.
2. Inspect the branch lifecycle and published state, then choose merge, rebase, or no-op for `upstream/main`.
3. Push normally after merge/no-op; use `--force-with-lease` only when rebasing an already-published branch.
4. Build the current branch's `multica` CLI, uninstall Homebrew `multica`, and install the binary to `~/.local/bin/multica` locally and on `my-mini`.
5. Update `dj:~/apps/multica` through `ssh my-mini` then `ssh dj`.
6. Run remote `./scripts/deploy.sh`.
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

## Important Judgement

- Treat remote `apps/web/next-env.d.ts` changes as Next build noise unless the user explicitly asks to track generated type path changes.
- Treat `package.json` `pnpm.onlyBuiltDependencies` additions as valuable when `pnpm install` or packaging needs native/postinstall dependencies such as `sharp`, `electron-winstaller`, `protobufjs`, `msw`, `core-js`, `unicode-animations`, or `unrs-resolver`.
- Do not use `git add .`. Stage only the files that belong to the requested release.
- If the remote working tree has a valuable change, copy it back locally, commit it on the current branch, apply the chosen upstream synchronization strategy, push, then update the remote checkout.
- If the remote working tree has only generated noise, leave it uncommitted or restore it only when it blocks a Git operation.
- If a rebase intentionally rewrites published history, the deployment checkout may no longer fast-forward. The script must stop rather than reset automatically; inspect the remote commits and only perform a manual reset after separately confirming the rewrite and target SHA.
- Never use a force push after merge. Never use plain `--force`; a published rebase requires `--force-with-lease`.

## Resolved Incident: Selfhost Build Host Pressure (2026-07-20)

The original outage was host-wide memory pressure, not proof that a build limit was absent. A cgroup `MemoryMax` only caps the build; it does not reserve memory for SSH, the reverse proxy, or unrelated resident services. The original `MemoryMax=5G` plus `MemorySwapMax=512M` budget was too large for a 7.5 GiB host already running OpenClaw, LiteLLM, Open Design, Hermes, and other services. Reclaim pressure made SSH and public HTTPS unresponsive before the user force-restarted `dj`, so that boot contains no conclusive kernel OOM record.

The protected retries established the safe operating boundary:

- The resource guard runs immediately before `systemd-run`, after dependency installation and migrations. During this release it refused two builds when post-install `MemAvailable` fell to 4396 MiB and 4484 MiB even though an earlier check had passed at 4718 MiB. Do not bypass this second check.
- `openclaw-gateway.service` was a user-level enabled unit and therefore restarted with `dj`; it used roughly 0.75-0.94 GiB. The unused gateway and the system-level `claw-visual.service` were disabled and stopped. `fwupd.service` was also stopped for the release window after confirming that it had no active work.
- With webpack memory optimizations enabled, a retry at `MemoryMax=3G` still reached exactly 3 GiB and was isolated by a memory-cgroup OOM at 15:15:43 CST. It ran for 5m43s and consumed 4m38s CPU without taking down the host. Evidence: `/var/tmp/multica-selfhost-release/cgroup-20260720T071000Z.log` and the kernel journal.
- After stopping the already-unavailable Multica frontend and then the backend for the deployment window, `MemAvailable=5790 MiB` passed the `4G + 1536 MiB` guard. The build succeeded with `MemoryHigh=4G`, `MemoryMax=4G`, `MemorySwapMax=256M`, and a 2048 MiB Node heap. It compiled webpack in 118s, completed TypeScript in 69s, and finished in 3m50s with 3m45s CPU. The cgroup peak was exactly 4 GiB, swap peak was 256 MiB, `oom_kill` stayed zero, and the host remained responsive. Evidence: `/var/tmp/multica-selfhost-release/cgroup-20260720T071925Z.log`.

The default safe budget is therefore `MemoryHigh=4G`, `MemoryMax=4G`, `MemorySwapMax=256M`, and a 1536 MiB host reserve. If the preflight cannot cover 5632 MiB or existing swap use exceeds 256 MiB, do not start the build. First inspect resident RSS and stop only confirmed-unused services; Multica frontend/backend may be stopped for the deployment window and restarted after the backend build.

Repeatable verification:

```bash
ssh my-mini 'ssh dj "cd /home/ubuntu/apps/multica && ./scripts/check-build-resources.sh"'
ssh my-mini 'ssh dj "tail -n 30 /var/tmp/multica-selfhost-release/cgroup-*.log; systemctl is-active multica-backend multica-frontend"'
ssh my-mini 'ssh dj "test -s /home/ubuntu/apps/multica/apps/web/.next/BUILD_ID && test -s /home/ubuntu/apps/multica/apps/web/.next/routes-manifest.json && test -s /home/ubuntu/apps/multica/apps/web/.next/prerender-manifest.json"'
curl --noproxy '*' --max-time 20 -I https://multica.zxyh.club/
```

The deploy script refuses to start the frontend build unless `MemAvailable` can cover `MemoryMax` plus the host reserve and existing swap usage is below its threshold. It uses the stable `multica-web-build.service` unit with `MemoryHigh`, `MemoryMax`, `MemorySwapMax`, and `OOMPolicy=kill`, and samples cgroup counters into `/var/tmp/multica-selfhost-release/cgroup-*.log` throughout the build so diagnostics survive an SSH disconnect or reboot.

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
scp server/bin/multica my-mini:~/.local/bin/multica
ssh my-mini 'zsh -lc "chmod 0755 ~/.local/bin/multica && ~/.local/bin/multica version"'
```

Update and deploy remote:

```bash
ssh my-mini 'ssh dj "cd ~/apps/multica && git fetch wingeddragon <branch> && git merge --ff-only wingeddragon/<branch> && ./scripts/deploy.sh"'
```

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
ssh my-mini 'ssh dj "cd ~/apps/multica && git status --short --branch && git rev-parse HEAD && systemctl is-active multica-backend multica-frontend"'
```

## Scripted Path

For the routine case where the local worktree is already clean and all changes are committed, run:

```bash
/Users/dong/.wtc/projects/multica/.agents/skills/multica-selfhost-release/scripts/run_release.sh
```

Useful environment overrides:

```bash
MULTICA_REPO=/Users/dong/.wtc/projects/multica
MULTICA_UPSTREAM_SYNC_STRATEGY=auto  # auto | merge | rebase
MULTICA_REMOTE_JUMP=my-mini
MULTICA_REMOTE_HOST=dj
MULTICA_REMOTE_DIR=/home/ubuntu/apps/multica
MULTICA_REMOTE_NAME=wingeddragon
MULTICA_WEB_BUILD_MAX_OLD_SPACE_SIZE_MB=2048
MULTICA_WEB_BUILD_MEMORY_HIGH=4G
MULTICA_WEB_BUILD_MEMORY_MAX=4G
MULTICA_WEB_BUILD_SWAP_MAX=256M
MULTICA_WEB_BUILD_HOST_RESERVE_MB=1536
MULTICA_WEB_BUILD_MAX_SWAP_USED_MB=256
MULTICA_BUILD_DIAGNOSTICS_DIR=/var/tmp/multica-selfhost-release
MULTICA_SKIP_DEPLOY=1
MULTICA_SKIP_PACKAGE=1
MULTICA_SKIP_INSTALL=1
MULTICA_SKIP_CLI_INSTALL=1
```

The script intentionally exits on a dirty local worktree, an invalid strategy, an indeterminate origin lookup, an origin branch with commits missing locally, or a non-fast-forward deployment checkout. Commit, integrate, or inspect deliberately first, then rerun it.
