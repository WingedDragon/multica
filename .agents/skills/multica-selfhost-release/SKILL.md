---
name: multica-selfhost-release
description: Deploy and package the self-hosted Multica app. Use when the user asks to update or upload the current Multica branch to dj:~/apps/multica, run scripts/deploy.sh through my-mini, build the macOS DMG with scripts/package.sh, replace /Applications/Multica.app, or repeat the full self-hosted release/install workflow.
---

# Multica Selfhost Release

Use this skill for the recurring self-hosted Multica release path:

1. Commit only task-related changes on the current branch.
2. Rebase on `upstream/main`.
3. Push the current branch to `origin`.
4. Update `dj:~/apps/multica` through `ssh my-mini` then `ssh dj`.
5. Run remote `./scripts/deploy.sh`.
6. Run local `./scripts/package.sh`.
7. Replace local `/Applications/Multica.app` with the generated app bundle.
8. Verify remote services and local app version/config.

## Important Judgement

- Treat remote `apps/web/next-env.d.ts` changes as Next build noise unless the user explicitly asks to track generated type path changes.
- Treat `package.json` `pnpm.onlyBuiltDependencies` additions as valuable when `pnpm install` or packaging needs native/postinstall dependencies such as `sharp`, `electron-winstaller`, `protobufjs`, `msw`, `core-js`, `unicode-animations`, or `unrs-resolver`.
- Do not use `git add .`. Stage only the files that belong to the requested release.
- If the remote working tree has a valuable change, copy it back locally, commit it on the current branch, rebase, push, then fast-forward the remote checkout.
- If the remote working tree has only generated noise, leave it uncommitted or restore it only when it blocks a Git operation.
- If a rebase rewrites the feature branch history, the deployment checkout may no longer fast-forward. Only reset the remote checkout after confirming the only remote dirty file is generated `apps/web/next-env.d.ts`; abort on any other remote local changes.

## Standard Workflow

Before running the full workflow:

```bash
cd /Users/dong/.wtc/projects/multica
git status --short --branch
git log --oneline --decorate -5
```

If there are uncommitted task changes, run targeted tests, stage only those files, then commit.

Rebase and push:

```bash
git fetch upstream main
git rebase upstream/main
git push --force-with-lease origin "$(git rev-parse --abbrev-ref HEAD)"
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
MULTICA_REMOTE_JUMP=my-mini
MULTICA_REMOTE_HOST=dj
MULTICA_REMOTE_DIR=/home/ubuntu/apps/multica
MULTICA_REMOTE_NAME=wingeddragon
MULTICA_SKIP_DEPLOY=1
MULTICA_SKIP_PACKAGE=1
MULTICA_SKIP_INSTALL=1
```

The script intentionally exits on a dirty local worktree. Commit deliberately first, then rerun it.
