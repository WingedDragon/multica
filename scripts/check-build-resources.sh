#!/usr/bin/env bash
set -euo pipefail

MEMINFO_PATH="${MULTICA_MEMINFO_PATH:-/proc/meminfo}"
WEB_BUILD_MEMORY_MAX="${MULTICA_WEB_BUILD_MEMORY_MAX:-3G}"
WEB_BUILD_HOST_RESERVE_MB="${MULTICA_WEB_BUILD_HOST_RESERVE_MB:-1536}"
WEB_BUILD_MAX_SWAP_USED_MB="${MULTICA_WEB_BUILD_MAX_SWAP_USED_MB:-256}"

size_to_kib() {
  local value=$1
  local number suffix

  if [[ ! "$value" =~ ^([0-9]+)([KMGTP]?)$ ]]; then
    echo "Invalid systemd memory size: $value" >&2
    return 2
  fi

  number="${BASH_REMATCH[1]}"
  suffix="${BASH_REMATCH[2]}"
  case "$suffix" in
    "") echo $(((number + 1023) / 1024)) ;;
    K) echo "$number" ;;
    M) echo $((number * 1024)) ;;
    G) echo $((number * 1024 * 1024)) ;;
    T) echo $((number * 1024 * 1024 * 1024)) ;;
    P) echo $((number * 1024 * 1024 * 1024 * 1024)) ;;
  esac
}

read_meminfo_kib() {
  local key=$1
  local value

  value="$(awk -v key="$key:" '$1 == key { print $2; exit }' "$MEMINFO_PATH")"
  if [[ ! "$value" =~ ^[0-9]+$ ]]; then
    echo "Missing or invalid $key in $MEMINFO_PATH" >&2
    return 2
  fi
  echo "$value"
}

if [[ ! "$WEB_BUILD_HOST_RESERVE_MB" =~ ^[0-9]+$ ]]; then
  echo "MULTICA_WEB_BUILD_HOST_RESERVE_MB must be an integer" >&2
  exit 2
fi
if [[ ! "$WEB_BUILD_MAX_SWAP_USED_MB" =~ ^[0-9]+$ ]]; then
  echo "MULTICA_WEB_BUILD_MAX_SWAP_USED_MB must be an integer" >&2
  exit 2
fi

memory_max_kib="$(size_to_kib "$WEB_BUILD_MEMORY_MAX")"
mem_available_kib="$(read_meminfo_kib MemAvailable)"
swap_total_kib="$(read_meminfo_kib SwapTotal)"
swap_free_kib="$(read_meminfo_kib SwapFree)"
reserve_kib=$((WEB_BUILD_HOST_RESERVE_MB * 1024))
max_swap_used_kib=$((WEB_BUILD_MAX_SWAP_USED_MB * 1024))
required_available_kib=$((memory_max_kib + reserve_kib))
swap_used_kib=$((swap_total_kib - swap_free_kib))

printf 'Frontend build resource preflight: MemAvailable=%s MiB, MemoryMax=%s MiB, host reserve=%s MiB, swap used=%s MiB\n' \
  "$((mem_available_kib / 1024))" \
  "$((memory_max_kib / 1024))" \
  "$WEB_BUILD_HOST_RESERVE_MB" \
  "$((swap_used_kib / 1024))"

if ((mem_available_kib < required_available_kib)); then
  printf 'Refusing frontend build: insufficient MemAvailable; need at least %s MiB for MemoryMax plus host reserve.\n' \
    "$((required_available_kib / 1024))" >&2
  exit 3
fi

if ((swap_used_kib > max_swap_used_kib)); then
  printf 'Refusing frontend build: swap usage is already too high; observed %s MiB, maximum %s MiB.\n' \
    "$((swap_used_kib / 1024))" \
    "$WEB_BUILD_MAX_SWAP_USED_MB" >&2
  exit 3
fi

echo "Frontend build resource preflight passed"
