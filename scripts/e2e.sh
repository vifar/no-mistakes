#!/usr/bin/env bash
# Suite wrapper for temporary E2E daemon ownership.
#
# Responsibilities:
#   1. Exact inventory directory (mode 0700) for this suite invocation
#   2. EXIT/INT/TERM trap that reaps only inventoried temp daemons
#   3. Concurrency cap via NM_E2E_DAEMON_MAX (default 2)
#   4. Pre-reap of any leftover inventory from a prior killed wrapper
#
# Honest boundary: this EXIT trap does NOT survive SIGKILL of this shell.
# When the wrapper itself is SIGKILL'd, the on-disk inventory is recovered
# on the next suite start (this script's pre-reap + package TestMain).
# Child go-test interruption/timeout/SIGKILL is covered: this shell still
# runs the trap and reaps.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

if [[ -z "${NM_E2E_DAEMON_INVENTORY:-}" ]]; then
  base="/tmp"
  if [[ -d /private/tmp ]]; then
    base="/private/tmp"
  fi
  NM_E2E_DAEMON_INVENTORY_PARENT="${base}/no-mistakes-e2e-inventories-$(id -u)"
  if [[ -L "$NM_E2E_DAEMON_INVENTORY_PARENT" ]]; then
    exit 1
  fi
  mkdir -p "$NM_E2E_DAEMON_INVENTORY_PARENT" || exit 1
  chmod 700 "$NM_E2E_DAEMON_INVENTORY_PARENT" || exit 1
  NM_E2E_DAEMON_INVENTORY="$(mktemp -d "${NM_E2E_DAEMON_INVENTORY_PARENT}/run-XXXXXX")" || exit 1
  export NM_E2E_DAEMON_INVENTORY
  export NM_E2E_DAEMON_INVENTORY_PARENT
  chmod 700 "$NM_E2E_DAEMON_INVENTORY" || exit 1
  printf '%s\n' "$$" >"$NM_E2E_DAEMON_INVENTORY/owner.pid" || exit 1
  chmod 600 "$NM_E2E_DAEMON_INVENTORY/owner.pid" || exit 1
  OWNED_INVENTORY=1
else
  mkdir -p "$NM_E2E_DAEMON_INVENTORY"
  chmod 700 "$NM_E2E_DAEMON_INVENTORY" 2>/dev/null || true
  OWNED_INVENTORY=0
fi

export NM_E2E_DAEMON_MAX="${NM_E2E_DAEMON_MAX:-2}"

reap_inventory() {
  # Best-effort; never expand into shared-daemon territory (reaper refuses).
  (cd "$ROOT" && go run ./internal/e2edaemon/reapmain.go) >/dev/null 2>&1 || true
}

if [[ -n "${NM_E2E_DAEMON_INVENTORY_PARENT:-}" ]]; then
  export NM_E2E_REAP_ABANDONED=1
  reap_inventory
  unset NM_E2E_REAP_ABANDONED
fi

cleanup_inventory() {
  reap_inventory
  if [[ "${OWNED_INVENTORY}" -eq 1 ]]; then
    rm -rf "$NM_E2E_DAEMON_INVENTORY" 2>/dev/null || true
  fi
}
trap cleanup_inventory EXIT INT TERM

# Default args match the historical Makefile e2e target; callers may override.
if [[ "$#" -eq 0 ]]; then
  # The default suite has one shared 2400s budget, including the journey
  # matrix. Each worker has its own process-wide test environment, while
  # daemon slots remain shared. The terminal-merge and retained-target tables share a worker beside the
  # remaining suite. Their measured durations balance the two workers without
  # adding serial tails or increasing the budget or daemon cap.
  budget_seconds=2400
  # Bash puts each asynchronous job in its own process group while monitor
  # mode is enabled. A group contains go test and its test binary descendants.
  set -m
  handle_signal() {
    local status="$1"
    local job_pid
    local job_pids=()
    trap - INT TERM
    while IFS= read -r job_pid; do
      [[ -n "$job_pid" ]] && job_pids+=("$job_pid")
    done < <(jobs -pr)
    for job_pid in "${job_pids[@]}"; do
      kill -TERM -- "-$job_pid" 2>/dev/null || true
    done
    for job_pid in "${job_pids[@]}"; do
      wait "$job_pid" 2>/dev/null || true
    done
    exit "$status"
  }
  trap 'handle_signal 130' INT
  trap 'handle_signal 143' TERM

  daemon_cap="${NM_E2E_DAEMON_MAX:-2}"
  if [[ "$daemon_cap" =~ ^\+?[0-9]+$ ]]; then
    daemon_cap="${daemon_cap#+}"
    while [[ "$daemon_cap" == 0* && "${#daemon_cap}" -gt 1 ]]; do
      daemon_cap="${daemon_cap#0}"
    done
    if [[ "$daemon_cap" == 0 ]]; then
      daemon_cap=2
    fi
  else
    daemon_cap=2
  fi

  if [[ "$daemon_cap" == 0 || "$daemon_cap" == 1 ]]; then
    go test -tags=e2e -count=1 -timeout "${budget_seconds}s" ./internal/e2e/... ./internal/pipeline/steps/... &
    serial_pid=$!
    wait "$serial_pid"
    serial_status=$?
    exit "$serial_status"
  fi

  start_seconds=$SECONDS
  go test -tags=e2e -count=1 -timeout "${budget_seconds}s" \
    -run '^TestUserJourney$/(claude|codex)$' ./internal/e2e/... &
  first_pid=$!
  go test -tags=e2e -count=1 -timeout "${budget_seconds}s" \
    -run '^TestUserJourney$' -skip '^TestUserJourney$/(claude|codex)$' ./internal/e2e/... &
  second_pid=$!

  first_status=0
  second_status=0
  wait "$first_pid" || first_status=$?
  wait "$second_pid" || second_status=$?
  if (( first_status != 0 || second_status != 0 )); then
    if (( first_status != 0 )); then
      exit "$first_status"
    fi
    exit "$second_status"
  fi

  elapsed_seconds=$((SECONDS - start_seconds))
  remaining_seconds=$((budget_seconds - elapsed_seconds))
  if (( remaining_seconds <= 0 )); then
    echo "e2e suite budget exhausted during the user-journey matrix" >&2
    exit 1
  fi

  go test -tags=e2e -count=1 -timeout "${remaining_seconds}s" \
    -run '^(TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$' ./internal/e2e/... &
  terminal_pid=$!
  go test -tags=e2e -count=1 -timeout "${remaining_seconds}s" \
    -skip '^(TestUserJourney|TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$' ./internal/e2e/... ./internal/pipeline/steps/... &
  remaining_pid=$!
  terminal_status=0
  remaining_status=0
  wait "$terminal_pid" || terminal_status=$?
  wait "$remaining_pid" || remaining_status=$?
  if (( terminal_status != 0 )); then
    exit "$terminal_status"
  fi
  exit "$remaining_status"
fi

go test "$@"
code=$?
exit "$code"
