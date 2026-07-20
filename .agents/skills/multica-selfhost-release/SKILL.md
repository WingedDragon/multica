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

## TODO: Diagnose Selfhost Build Host Pressure

This TODO is active after the 2026-07-20 `dj` incident. Do not silently skip it on the next invocation of this skill.

Observed facts:

- `dj` has about 7.5 GiB RAM and 1.9 GiB swap, with several non-Multica services consuming memory.
- The frontend build completed webpack compilation and became unresponsive during `Running TypeScript` while using `MemoryMax=5G` and `MemorySwapMax=512M`.
- New SSH connections timed out during banner exchange and public HTTPS timed out. The user then force-restarted `dj` at about 14:34 CST.
- The previous boot recorded the build unit start but no OOM kill for this attempt, so the exact build cgroup peak and final pressure source remain unknown. Do not report a specific OOM cause without new evidence.
- The interrupted in-place build left `.next` without `BUILD_ID`, `routes-manifest.json`, or `prerender-manifest.json`, causing the frontend service to restart and return HTTP 502 after reboot.

On the next invocation, treat this as release work that must be diagnosed, handled, and recorded before closing the release:

1. Read `/var/tmp/multica-selfhost-release/preflight-*.log` and `cgroup-*.log`. Compare `MemAvailable`, configured `MemoryMax`, the host reserve, swap usage, top RSS processes, cgroup `memory.current` / `memory.peak` / `memory.swap.current`, and `memory.events`.
2. Inspect the stable unit with `journalctl -u multica-web-build.service`, `systemctl show multica-web-build.service`, and, after a reboot, `journalctl -b -1 -u multica-web-build.service`.
3. Inspect kernel evidence with `journalctl -k` or `journalctl -b -1 -k` for `oom`, `Killed process`, `memory cgroup`, `watchdog`, and hung tasks. Use `last -x` and the user's report to distinguish a forced reboot from a kernel-initiated reboot.
4. Verify `.next` completeness and `systemctl is-active multica-backend multica-frontend` before calling the service healthy.
5. If the resource preflight refuses the build, do not override it blindly. Identify which resident services or build phase consume the margin, choose a safe remediation, and record the evidence and result in this section.
6. When the exact cause is proven, replace this TODO with the conclusion, measured peak, chosen safe budget, and a repeatable verification command.

The deploy script now refuses to start the frontend build unless `MemAvailable` can cover `MemoryMax` plus a host reserve and existing swap usage is below its threshold. It uses the stable `multica-web-build.service` unit with `MemoryHigh`, `MemoryMax`, `MemorySwapMax`, and `OOMPolicy=kill`, and samples cgroup counters into `/var/tmp/multica-selfhost-release/cgroup-*.log` throughout the build so later diagnostics survive an SSH disconnect or reboot.

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
MULTICA_WEB_BUILD_MEMORY_HIGH=2560M
MULTICA_WEB_BUILD_MEMORY_MAX=3G
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
