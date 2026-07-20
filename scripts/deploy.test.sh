#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY_SCRIPT="$ROOT_DIR/scripts/deploy.sh"

require_line() {
  local expected=$1

  if ! grep -Fq -- "$expected" "$DEPLOY_SCRIPT"; then
    echo "Missing expected safe frontend build setting:"
    echo "  $expected"
    exit 1
  fi
}

require_line 'MULTICA_WEB_BUILD_MAX_OLD_SPACE_SIZE_MB:-3072'
require_line 'MULTICA_WEB_BUILD_MEMORY_MAX:-5G'
require_line 'MULTICA_WEB_BUILD_SWAP_MAX:-512M'
require_line '-p "CPUQuota=100%"'
require_line '-p "Nice=10"'
require_line '-p "IOWeight=50"'

echo "self-host deploy build budget ok"
