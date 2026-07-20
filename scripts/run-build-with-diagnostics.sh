#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -lt 2 ]; then
  echo "Usage: $0 <diagnostics-file> <command> [args...]" >&2
  exit 2
fi

diagnostics_file=$1
shift
sample_interval="${MULTICA_BUILD_SAMPLE_INTERVAL_SECONDS:-5}"

if [ -n "${MULTICA_CGROUP_ROOT:-}" ]; then
  cgroup_root="$MULTICA_CGROUP_ROOT"
else
  cgroup_path="$(awk -F: '$1 == "0" { print $3; exit }' /proc/self/cgroup)"
  if [ -z "$cgroup_path" ]; then
    echo "Unable to determine the current cgroup v2 path" >&2
    exit 2
  fi
  cgroup_root="/sys/fs/cgroup$cgroup_path"
fi

mkdir -p "$(dirname "$diagnostics_file")"

sample_cgroup() {
  local metric value

  printf 'timestamp=%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')" >>"$diagnostics_file"
  for metric in memory.current memory.peak memory.swap.current; do
    if [ -r "$cgroup_root/$metric" ]; then
      value="$(tr -d '\n' <"$cgroup_root/$metric")"
    else
      value="unavailable"
    fi
    printf '%s=%s\n' "${metric//./_}" "$value" >>"$diagnostics_file"
  done
  if [ -r "$cgroup_root/memory.events" ]; then
    cat "$cgroup_root/memory.events" >>"$diagnostics_file"
  fi
  if [ -r "$cgroup_root/memory.pressure" ]; then
    cat "$cgroup_root/memory.pressure" >>"$diagnostics_file"
  fi
  printf '\n' >>"$diagnostics_file"
}

"$@" &
command_pid=$!

monitor_cgroup() {
  while kill -0 "$command_pid" >/dev/null 2>&1; do
    sample_cgroup
    sleep "$sample_interval"
  done
}

monitor_cgroup &
monitor_pid=$!

command_status=0
wait "$command_pid" || command_status=$?
kill "$monitor_pid" >/dev/null 2>&1 || true
wait "$monitor_pid" >/dev/null 2>&1 || true
sample_cgroup

exit "$command_status"
