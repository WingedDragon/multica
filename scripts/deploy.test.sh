#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DEPLOY_SCRIPT="$ROOT_DIR/scripts/deploy.sh"
RESOURCE_CHECK_SCRIPT="$ROOT_DIR/scripts/check-build-resources.sh"
BUILD_DIAGNOSTICS_SCRIPT="$ROOT_DIR/scripts/run-build-with-diagnostics.sh"

require_line() {
  local expected=$1

  if ! grep -Fq -- "$expected" "$DEPLOY_SCRIPT"; then
    echo "Missing expected safe frontend build setting:"
    echo "  $expected"
    exit 1
  fi
}

require_line 'MULTICA_WEB_BUILD_MAX_OLD_SPACE_SIZE_MB:-2048'
require_line 'MULTICA_WEB_BUILD_MEMORY_HIGH:-4G'
require_line 'MULTICA_WEB_BUILD_MEMORY_MAX:-4G'
require_line 'MULTICA_WEB_BUILD_SWAP_MAX:-256M'
require_line 'MULTICA_WEB_BUILD_HOST_RESERVE_MB:-1536'
require_line 'check-build-resources.sh'
require_line 'run-build-with-diagnostics.sh'
require_line '--unit="$WEB_BUILD_UNIT"'
require_line '-p "MemoryHigh=${WEB_BUILD_MEMORY_HIGH}"'
require_line '-p "OOMPolicy=kill"'
require_line '-p "CPUQuota=100%"'
require_line '-p "Nice=10"'
require_line '-p "IOWeight=50"'

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

write_meminfo() {
  local path=$1
  local available_kb=$2
  local swap_total_kb=$3
  local swap_free_kb=$4

  cat >"$path" <<EOF
MemTotal:        7864320 kB
MemFree:         1048576 kB
MemAvailable:    $available_kb kB
SwapTotal:       $swap_total_kb kB
SwapFree:        $swap_free_kb kB
EOF
}

run_resource_check() {
  MULTICA_MEMINFO_PATH="$1" \
  MULTICA_WEB_BUILD_MEMORY_MAX=4G \
  MULTICA_WEB_BUILD_HOST_RESERVE_MB=1536 \
  MULTICA_WEB_BUILD_MAX_SWAP_USED_MB=256 \
  "$RESOURCE_CHECK_SCRIPT"
}

write_meminfo "$tmp/safe.meminfo" 6291456 2097152 2097152
run_resource_check "$tmp/safe.meminfo" >"$tmp/safe.out" 2>"$tmp/safe.err"
grep -Fq 'Frontend build resource preflight passed' "$tmp/safe.out"

write_meminfo "$tmp/low-memory.meminfo" 4500000 2097152 2097152
if run_resource_check "$tmp/low-memory.meminfo" >"$tmp/low-memory.out" 2>"$tmp/low-memory.err"; then
  echo "resource preflight should reject insufficient MemAvailable" >&2
  exit 1
fi
grep -Fq 'Refusing frontend build: insufficient MemAvailable' "$tmp/low-memory.err"

write_meminfo "$tmp/swap-pressure.meminfo" 6291456 2097152 1048576
if run_resource_check "$tmp/swap-pressure.meminfo" >"$tmp/swap-pressure.out" 2>"$tmp/swap-pressure.err"; then
  echo "resource preflight should reject existing swap pressure" >&2
  exit 1
fi
grep -Fq 'Refusing frontend build: swap usage is already too high' "$tmp/swap-pressure.err"

mkdir -p "$tmp/cgroup"
printf '1048576\n' >"$tmp/cgroup/memory.current"
printf '2097152\n' >"$tmp/cgroup/memory.peak"
printf '0\n' >"$tmp/cgroup/memory.swap.current"
cat >"$tmp/cgroup/memory.events" <<'EOF'
low 0
high 2
max 0
oom 0
oom_kill 0
EOF

MULTICA_CGROUP_ROOT="$tmp/cgroup" \
MULTICA_BUILD_SAMPLE_INTERVAL_SECONDS=0.05 \
  "$BUILD_DIAGNOSTICS_SCRIPT" "$tmp/cgroup.log" bash -c 'sleep 0.15'
grep -Fq 'memory_current=1048576' "$tmp/cgroup.log"
grep -Fq 'memory_peak=2097152' "$tmp/cgroup.log"
grep -Fq 'memory_swap_current=0' "$tmp/cgroup.log"
grep -Fq 'oom_kill 0' "$tmp/cgroup.log"

diagnostic_status=0
MULTICA_CGROUP_ROOT="$tmp/cgroup" \
MULTICA_BUILD_SAMPLE_INTERVAL_SECONDS=0.05 \
  "$BUILD_DIAGNOSTICS_SCRIPT" "$tmp/cgroup-failure.log" bash -c 'exit 23' || diagnostic_status=$?
if [ "$diagnostic_status" -ne 23 ]; then
  echo "build diagnostics wrapper must preserve command exit status" >&2
  exit 1
fi

echo "self-host deploy resource guard ok"
