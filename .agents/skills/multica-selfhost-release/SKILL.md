---
name: multica-selfhost-release
description: This skill should be used when the user asks to "deploy Multica self-hosted", "sync upstream/main", "update dj", "run a selfhost release", "package Multica desktop", or "merge or rebase a selfhost branch". It deploys through direct SSH to dj and packages the self-hosted Multica app.
---

# Multica Selfhost Release

Use this skill for the recurring self-hosted Multica release path:

1. Commit only task-related changes on the current branch.
2. Inspect the branch lifecycle and published state, then choose merge, rebase, or no-op for `upstream/main`.
3. Push normally after merge/no-op; use `--force-with-lease` only when rebasing an already-published branch.
4. Build the current branch's `multica` CLI, uninstall Homebrew `multica`, and install the binary to `~/.local/bin/multica` locally and on `my-mini`.
5. Update `dj:~/apps/multica` through direct local `ssh dj`.
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

`dj` is directly reachable from this Mac with `ssh dj`; use that route for the deployment checkout, protected release window, and remote verification. `my-mini` remains only the target for the separately installed and signed CLI.

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

## Resolved Incident: Selfhost Build Host Pressure

The build is isolated because a cgroup `MemoryMax` caps the build but does not reserve capacity for SSH, the reverse proxy, or resident services. Keep the resource guard immediately before `systemd-run`; do not bypass its post-install availability or swap checks.

The verified release configuration as of 2026-08-17 is:

- `MULTICA_WEB_BUILD_MAX_OLD_SPACE_SIZE_MB=1792`
- `MULTICA_WEB_BUILD_MEMORY_HIGH=4G`
- `MULTICA_WEB_BUILD_MEMORY_MAX=4608M`
- `MULTICA_WEB_BUILD_SWAP_MAX=256M`
- `MULTICA_WEB_BUILD_HOST_RESERVE_MB=1536`
- `MULTICA_WEB_BUILD_MAX_SWAP_USED_MB=256`

This configuration built `2f44ca05b` successfully after removing the retired LiteLLM container, and it must be the default in `run_release.sh`. The `litellm` container has been removed permanently; do not inspect, stop, start, or restore it during release windows.

The deployment window records only whether `multica-backend` and `multica-frontend` were active, installs its `EXIT` trap before stopping either, resets stale swap with `sudo swapoff -a && sudo swapon -a`, then allows `deploy.sh` to enforce the resource guard. The trap restores only the services that were active before the attempt.

If the preflight cannot cover the configured `MemoryMax` plus the 1536 MiB host reserve, or used swap exceeds 256 MiB, do not start the build. Inspect confirmed resident processes or reduce the build footprint; do not weaken the guard merely to start a build.

Repeatable verification:

```bash
ssh dj 'cd /home/ubuntu/apps/multica && ./scripts/check-build-resources.sh'
ssh dj 'tail -n 30 /var/tmp/multica-selfhost-release/cgroup-*.log; systemctl is-active multica-backend multica-frontend'
ssh dj 'test -s /home/ubuntu/apps/multica/apps/web/.next/BUILD_ID && test -s /home/ubuntu/apps/multica/apps/web/.next/routes-manifest.json && test -s /home/ubuntu/apps/multica/apps/web/.next/prerender-manifest.json'
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
scp server/bin/multica my-mini:~/.local/bin/multica.upload
ssh my-mini 'zsh -lc "chmod 0755 ~/.local/bin/multica.upload && codesign --force --sign - ~/.local/bin/multica.upload && mv ~/.local/bin/multica.upload ~/.local/bin/multica && codesign --verify --strict ~/.local/bin/multica && ~/.local/bin/multica version"'
```

Update and deploy remote through direct local `ssh dj` in a protected release window. Preserve the pre-release Multica service state and restore it with an `EXIT` trap; use `scripts/run_release.sh` for the exact implementation rather than copying a partial inline command. The core sequence is:

1. Fast-forward the remote checkout to the pinned final `HEAD` and verify the SHA.
2. Record `multica-backend` and `multica-frontend` state, install the cleanup trap, then stop those services.
3. Reset stale swap, run `./scripts/deploy.sh` with the proven build budget, and allow its resource guard to refuse unsafe builds.
4. Restore every Multica service that was active before the release, whether deployment succeeds or fails.

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
MULTICA_WEB_BUILD_MAX_OLD_SPACE_SIZE_MB=1792
MULTICA_WEB_BUILD_MEMORY_HIGH=4G
MULTICA_WEB_BUILD_MEMORY_MAX=4608M
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
