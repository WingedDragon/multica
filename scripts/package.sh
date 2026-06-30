#!/usr/bin/env bash
# One-shot packaging for the Multica Desktop app, pointing at the
# self-hosted backend at https://multica.zxyh.club/.
#
# Endpoint resolution: the production renderer reads runtime config from
# `~/.multica/desktop.json` on the *running* machine — it does NOT bake
# VITE_* env vars into the bundle (see runtime-config-loader.ts:18 — env
# is only consulted when `is.dev` is true). So the correct way to ship a
# self-hosted build is to drop a `desktop.json` into the user's home dir
# on every machine that runs the app. This script writes that file on
# the build host so a local-built-then-locally-installed app just works;
# distributing the app to other machines additionally requires placing
# the same file on each target machine before first launch.
#
# Extra args are forwarded to `pnpm package` (e.g. --mac --arm64).

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"

REMOTE_HOST="multica.zxyh.club"
API_URL="https://${REMOTE_HOST}"
WS_URL="wss://${REMOTE_HOST}/ws"
APP_URL="https://${REMOTE_HOST}"

DESKTOP_CONFIG_DIR="${HOME}/.multica"
DESKTOP_CONFIG_FILE="${DESKTOP_CONFIG_DIR}/desktop.json"

cd "$REPO_ROOT"

# Use npmmirror for binary downloads — GitHub releases are routinely
# unreachable from CN networks and downloads fail with TLS socket
# disconnects otherwise. Two independent, easily-confused download paths
# each need a mirror:
#
#   1. ELECTRON_MIRROR — the electron RUNTIME, pulled by electron's
#      postinstall during `pnpm install`.
#   2. ELECTRON_BUILDER_BINARIES_MIRROR — electron-builder's TOOL binaries
#      (dmgbuild-bundle, winCodeSign, …), pulled during packaging from the
#      electron-builder-binaries repo.
#
# THE TRAP (this is what caused the dmgbuild-bundle 404): electron-builder
# fetches its tool binaries through @electron/get, and @electron/get's mirror
# resolution ranks the ELECTRON_MIRROR env var ABOVE the binaries mirror that
# electron-builder passes in (see @electron/get artifact-utils.js `mirrorVar`:
# `ELECTRON_MIRROR` wins over `mirrorOptions.mirror`). So while ELECTRON_MIRROR
# is set, dmgbuild-bundle is requested from the electron RUNTIME mirror —
#   <electron-mirror>/dmg-builder@1.2.0/dmgbuild-bundle-*.tar.gz  → 404
# — because that file only exists under the electron-builder-binaries mirror.
#
# So we set ELECTRON_MIRROR for the install step but `unset` it right before
# packaging (below), letting @electron/get fall back to the binaries mirror
# for the tool downloads. The electron runtime is cached by then, so dropping
# ELECTRON_MIRROR for packaging costs nothing.
export ELECTRON_MIRROR="${ELECTRON_MIRROR:-https://npmmirror.com/mirrors/electron/}"
export ELECTRON_BUILDER_BINARIES_MIRROR="${ELECTRON_BUILDER_BINARIES_MIRROR:-https://npmmirror.com/mirrors/electron-builder-binaries/}"

# ---------- Prerequisites ----------
for cmd in node pnpm curl unzip; do
  if ! command -v "$cmd" >/dev/null 2>&1; then
    echo "✗ Missing prerequisite: $cmd" >&2
    exit 1
  fi
done

# ---------- Install workspace dependencies ----------
# `pnpm package` invokes `electron-vite` / `electron-builder` from the
# desktop workspace's node_modules; without an install, the script
# fails with `electron-vite: command not found`.
#
# `--shamefully-hoist` is required for the desktop renderer build:
# apps/desktop/src/renderer/src/App.tsx imports `@tanstack/react-query`
# directly, but apps/desktop/package.json never declares it — it's only
# a transitive dep via @multica/core / @multica/views. The repo's
# `.npmrc` has `shamefully-hoist=true` to flatten it into the root
# node_modules so the desktop bundler can resolve it. pnpm 11 NO LONGER
# reads pnpm-specific settings from `.npmrc` (only auth/registry), so the
# isolated linker hides the transitive package and the Rolldown build
# fails with: cannot resolve "@tanstack/react-query" from App.tsx.
#
# We pass the flag on the CLI here instead of editing the tracked
# `.npmrc` / `pnpm-workspace.yaml` / `apps/desktop/package.json`, keeping
# this self-hosted packaging workaround fully contained in this script.
# Sentinel for "is the flat hoist present": root node_modules has the
# hoisted @tanstack/react-query (absent under pnpm 11's default linker).
if [ ! -x "$REPO_ROOT/apps/desktop/node_modules/.bin/electron-vite" ] \
   || [ ! -x "$REPO_ROOT/apps/desktop/node_modules/.bin/electron-builder" ] \
   || [ ! -d "$REPO_ROOT/node_modules/@tanstack/react-query" ]; then
  echo "==> Installing workspace dependencies (pnpm install --shamefully-hoist)"
  pnpm install --shamefully-hoist
fi

# electron-builder's mac pack step reads the Electron runtime zip from
# ~/Library/Caches/electron. A truncated cache can make unpack-electron appear
# successful and then fail much later while renaming Contents/MacOS/Electron.
# Validate it before we repurpose ELECTRON_MIRROR for builder tool binaries.
refresh_electron_runtime_cache() {
  local electron_version host_platform host_arch cache_dir cache_file tmp bad

  electron_version="$(node -p 'require("./apps/desktop/node_modules/electron/package.json").version')"
  host_platform="$(node -p 'process.platform')"
  host_arch="$(node -p 'process.arch')"

  if [ "$host_platform" != "darwin" ]; then
    return
  fi

  cache_dir="${HOME}/Library/Caches/electron"
  cache_file="${cache_dir}/electron-v${electron_version}-${host_platform}-${host_arch}.zip"
  mkdir -p "$cache_dir"

  if [ -f "$cache_file" ] && unzip -tq "$cache_file" >/dev/null 2>&1; then
    return
  fi

  if [ -f "$cache_file" ]; then
    bad="${cache_file}.corrupt-$(date +%Y%m%d%H%M%S)"
    mv "$cache_file" "$bad"
    echo "==> Corrupt Electron cache moved to $bad"
  else
    echo "==> Electron cache missing: $cache_file"
  fi

  tmp="${cache_file}.tmp-$$"
  rm -f "$tmp"
  echo "==> Downloading Electron runtime cache from ${ELECTRON_MIRROR}"
  curl -fL --retry 3 --retry-delay 2 --connect-timeout 20 \
    "${ELECTRON_MIRROR%/}/v${electron_version}/electron-v${electron_version}-${host_platform}-${host_arch}.zip" \
    -o "$tmp"
  unzip -tq "$tmp" >/dev/null
  mv "$tmp" "$cache_file"
  echo "==> Electron cache verified: $cache_file"
}

refresh_electron_runtime_cache

# ---------- Runtime config (~/.multica/desktop.json) ----------
# This is the tracked, supported override mechanism for production builds
# (parseRuntimeConfig in apps/desktop/src/shared/runtime-config.ts). It is
# NOT bundled into the .app — it lives in the user's home directory and
# is read on every launch.
mkdir -p "$DESKTOP_CONFIG_DIR"
if [ -f "$DESKTOP_CONFIG_FILE" ]; then
  echo "==> Existing $DESKTOP_CONFIG_FILE detected — leaving untouched"
else
  cat > "$DESKTOP_CONFIG_FILE" <<EOF
{
  "schemaVersion": 1,
  "apiUrl": "${API_URL}",
  "wsUrl": "${WS_URL}",
  "appUrl": "${APP_URL}"
}
EOF
  echo "==> Wrote $DESKTOP_CONFIG_FILE"
fi

echo "==> Packaging Multica Desktop (self-hosted)"
echo "    API : ${API_URL}"
echo "    WS  : ${WS_URL}"
echo "    APP : ${APP_URL}"
echo ""

# ---------- Build & package ----------
# Self-hosted builds use ad-hoc codesigning (`identity: '-'`) instead of an
# Apple Developer ID cert. The binary gets a valid signature so Gatekeeper
# on the build machine is happy, but it is not notarized and end users will
# need to right-click → Open (or remove the quarantine xattr) on first run.
# Override electron-builder's tracked config without editing the file.
export CSC_IDENTITY_AUTO_DISCOVERY=false

# electron-builder 26.x downloads dmgbuild-bundle through @electron/get's
# generic artifact path. That path ignores ELECTRON_BUILDER_BINARIES_MIRROR,
# but still honors ELECTRON_MIRROR. The Electron runtime cache was verified
# above, so pointing ELECTRON_MIRROR at the builder-binaries mirror here keeps
# dmgbuild-bundle off GitHub without breaking runtime unpacking.
export ELECTRON_MIRROR="${ELECTRON_BUILDER_BINARIES_MIRROR}"

# Note: the desktop wrapper (apps/desktop/scripts/package.mjs) already adds
# `-c.mac.notarize=false` when APPLE_TEAM_ID is unset, so don't pass it again
# here — duplicates parse as a string array and fail boolean validation.
pnpm --filter @multica/desktop package \
  -c.mac.identity=- \
  "$@"

echo ""
echo "✓ Done. Artifacts: apps/desktop/dist/"
echo ""
echo "Note: distributing this build to another machine?"
echo "  Copy ${DESKTOP_CONFIG_FILE} to the target user's \$HOME/.multica/"
echo "  before first launch, otherwise the app falls back to the cloud defaults."
