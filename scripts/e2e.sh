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
  # daemon slots remain shared.
  budget_seconds=2400
  # The remaining bucket is split across parallel shards, each with its own
  # go test process. A single process for the whole bucket outgrew the
  # budget: its slowest journeys plus everything queued behind them exceeded
  # the shared 2400s, so the phase died on its own -timeout rather than on a
  # failing test. Sharding by test name gives every shard its own timeout, so
  # one slow journey can no longer consume the whole bucket's allowance, and
  # the shards finish in the time of the slowest one instead of their sum.
  #
  # The shard count is deliberately small: each shard is a whole go test
  # process with its own daemon slots, and the daemon cap is shared, so more
  # shards than the cap supports only adds contention. It is a measured
  # count, not a knob: an unsupported value could skip shards or break the
  # arithmetic that places the long poles.
  shard_count=3
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

  # Discover the test names of EVERY package the shards run, not just the e2e
  # package. A shard's -run pattern selects the tests that run in it, so a
  # package whose names are missing from the pattern would silently run
  # nothing at all - the step tests would stop running without a single
  # failure.
  #
  # A discovery that fails is fatal, and the failure is the EXIT STATUS of go
  # test, not whether the output happens to be empty. One package under
  # steps/... can list successfully while another times out during
  # discovery, and an empty check would let the shards succeed without ever
  # running the omitted tests.
  #
  # The discovery runs BEFORE any worker starts, so a failure here leaves no
  # worker behind to report a result the discovery already invalidated.
  all_tests=""
  for package in ./internal/e2e/... ./internal/pipeline/steps/...; do
    listed="$(go test -tags=e2e -count=1 -timeout 120s -list '.*' "$package" 2>/dev/null)"
    status=$?
    if (( status != 0 )); then
      echo "e2e suite could not discover the tests of $package (exit $status)" >&2
      exit 1
    fi
    listed="$(printf '%s\n' "$listed" | grep '^Test')"
    if [[ -z "$listed" ]]; then
      echo "e2e suite discovered no tests in $package" >&2
      exit 1
    fi
    all_tests="${all_tests}${listed}
"
  done

  # The budget is recomputed AFTER the discovery, because the discovery is
  # part of the suite's own time: measuring it before would hand the workers
  # an allowance that already includes the time the discovery took.
  elapsed_seconds=$((SECONDS - start_seconds))
  remaining_seconds=$((budget_seconds - elapsed_seconds))
  if (( remaining_seconds <= 0 )); then
    echo "e2e suite budget exhausted before the workers started" >&2
    exit 1
  fi

  # The terminal-merge and retained-target journeys keep their own worker: they
  # are the two slowest live journeys, and sharing a worker with the rest of
  # the bucket is what made that worker the suite's long pole.
  go test -tags=e2e -count=1 -timeout "${remaining_seconds}s" \
    -run '^(TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$' ./internal/e2e/... &
  terminal_pid=$!

  # Everything else is split across shards. Each shard runs its own go test
  # process with its own timeout, so a slow journey in one shard cannot eat
  # the allowance of the others, and the bucket costs the time of its slowest
  # shard rather than the sum of all of them.
  #
  # The split balances by MEASURED TIME, not by test count. The journeys
  # below are the measured long pole of the bucket - each one costs more than
  # the rest of a shard together - so they are handed out one per shard
  # explicitly. Everything else is spread by a checksum of the test's own
  # name, which is stable across runs, so a test always lands in the same
  # shard and a failure reproduces in the same place.
  slow_journeys=(
    TestAttachCommitStatusJourney
    TestTargetAdvanceAcceptedGapComparisonOutageLiveJourney
    TestCIReplayOptionCannotBypassMovedComparisonOnRealBinary
    TestEvalJourney
  )

  # Every measured long pole is placed, round robin over the shards, so a
  # shard count that does not match the number of long poles still places each
  # one of them exactly once.
  shard_slow=()
  slow_index=0
  while (( slow_index < ${#slow_journeys[@]} )); do
    target=$((slow_index % shard_count))
    if [[ -z "${shard_slow[$target]-}" ]]; then
      shard_slow[$target]="${slow_journeys[$slow_index]}"
    else
      shard_slow[$target]="${shard_slow[$target]}
${slow_journeys[$slow_index]}"
    fi
    slow_index=$((slow_index + 1))
  done

  shard_pids=()
  shard_statuses=()
  shard_index=0
  while (( shard_index < shard_count )); do
    shard_tests="${shard_slow[$shard_index]-}"
    # The rest of the shard is the remaining tests whose name hashes into it.
    rest="$(printf '%s\n' "$all_tests" | awk -v shard="$shard_index" -v count="$shard_count" -v slow="${slow_journeys[*]}" '
      BEGIN { n = split(slow, names, " "); for (i = 1; i <= n; i++) skip[names[i]] = 1 }
      {
        name = $0
        if (name in skip) { next }
        checksum = 0
        for (i = 1; i <= length(name); i++) {
          checksum = (checksum * 31 + index("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", substr(name, i, 1))) % 1000003
        }
        if (checksum % count == shard) { print name }
      }')"
    if [[ -n "$rest" ]]; then
      if [[ -n "$shard_tests" ]]; then
        shard_tests="$shard_tests
$rest"
      else
        shard_tests="$rest"
      fi
    fi
    if [[ -z "$shard_tests" ]]; then
      shard_index=$((shard_index + 1))
      continue
    fi
    shard_pattern="$(printf '%s\n' "$shard_tests" | paste -sd'|' -)"
    go test -tags=e2e -count=1 -timeout "${remaining_seconds}s" \
      -run "^(${shard_pattern})$" \
      -skip '^(TestUserJourney|TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$' \
      ./internal/e2e/... ./internal/pipeline/steps/... &
    shard_pids+=("$!")
    shard_statuses+=(0)
    shard_index=$((shard_index + 1))
  done

  terminal_status=0
  wait "$terminal_pid" || terminal_status=$?
  shard_index=0
  while (( shard_index < ${#shard_pids[@]} )); do
    wait "${shard_pids[$shard_index]}" || shard_statuses[$shard_index]=$?
    shard_index=$((shard_index + 1))
  done
  if (( terminal_status != 0 )); then
    exit "$terminal_status"
  fi
  shard_index=0
  while (( shard_index < ${#shard_statuses[@]} )); do
    if (( shard_statuses[$shard_index] != 0 )); then
      exit "${shard_statuses[$shard_index]}"
    fi
    shard_index=$((shard_index + 1))
  done
  exit 0
fi

go test "$@"
code=$?
exit "$code"
