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

# The concurrency cap bounds how many TEMPORARY DAEMONS may be live at
# once. It is not a bound on test parallelism: the step-test worker takes no
# daemon slot at all, and the shards each take one.
#
# The default is four, not two, because the e2e bucket is roughly 4000
# seconds of measured work and the budget is 2400. With a cap of two only
# one shard could run beside the terminal worker, so the bucket was
# serialised into a single worker that could not finish inside the budget -
# the suite died on its own -timeout rather than on a failing test.
export NM_E2E_DAEMON_MAX="${NM_E2E_DAEMON_MAX:-4}"

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

# place_shards assigns every discovered test to exactly one shard, balancing
# by the journey's MEASURED cost.
#
# The rule is longest-processing-time-first: the most expensive journey goes
# to the least loaded shard, one at a time, and the rest follow in the same
# order. The result is a function of the measurements alone, so it is
# deterministic - a journey does not move when an unrelated one is added, and
# a failure reproduces in the same place.
#
# The measured costs live in scripts/e2e-journey-costs.txt. A test that is
# not listed there costs the default, which is small on purpose: an
# UNMEASURED test is expected to be an ordinary one, and a missing entry must
# not let it dominate a shard.
place_shards() {
  local costs_file="$ROOT/scripts/e2e-journey-costs.txt"
  if [[ ! -f "$costs_file" ]]; then
    echo "e2e suite is missing its measured journey costs: $costs_file" >&2
    exit 1
  fi
  local default_cost
  default_cost="$(awk -F= '/^default_journey_seconds=/{print $2; exit}' "$costs_file")"
  if [[ -z "$default_cost" ]]; then
    echo "e2e suite could not read the default journey cost" >&2
    exit 1
  fi

  # The cost of every test: the measured ones from the costs file, and the
  # default for the rest. The costs are read into a temp file rather than
  # passed as an awk variable, because a shell variable cannot carry the
  # newlines awk would then have to unescape.
  local cost_file
  cost_file="$(mktemp)"
  awk '
    /^journey_seconds=\(/ { in_list = 1; next }
    in_list && /^\)/ { in_list = 0; next }
    in_list { print $1, $2; next }
  ' "$costs_file" >"$cost_file"
  if [[ ! -s "$cost_file" ]]; then
    rm -f "$cost_file"
    echo "e2e suite read no measured journey costs" >&2
    exit 1
  fi

  # The discovered tests, most expensive first. The sort is by the measured
  # cost and then by name, so the order is total and deterministic.
  #
  # Only the E2E journeys are placed. The pipeline-step tests have their own
  # worker, and the journeys the terminal worker owns run in that worker; a
  # test placed in a shard and run by another worker would run twice.
  local placed
  placed="$(printf '%s\n' "$all_tests" | awk -v costs="$cost_file" -v fallback="$default_cost" '
    BEGIN {
      while ((getline line < costs) > 0) {
        split(line, fields, " ")
        if (fields[1] != "" && fields[2] != "") { cost[fields[2]] = fields[1] + 0 }
      }
    }
    {
      name = $0
      if (name == "TestUserJourney" || name == "TestTargetAdvanceTerminalMergeLiveJourney" || name == "TestRetainedTargetContinuityLiveJourney") { next }
      c = (name in cost) ? cost[name] : fallback + 0
      printf "%d\t%s\n", c, name
    }' | sort -rn -k1,1 -k2,2 | cut -f2-)"

  local name target cost
  while IFS= read -r name; do
    [[ -z "$name" ]] && continue
    cost="$(awk -v want="$name" '$2 == want { print $1; exit }' "$cost_file")"
    if [[ -z "$cost" ]]; then
      cost="$default_cost"
    fi
    # The least loaded shard takes it.
    target=0
    local candidate
    for (( candidate = 1; candidate < shard_count; candidate++ )); do
      if (( shard_load[$candidate] < shard_load[$target] )); then
        target=$candidate
      fi
    done
    if [[ -z "${shard_tests[$target]}" ]]; then
      shard_tests[$target]="$name"
    else
      shard_tests[$target]="${shard_tests[$target]}
$name"
    fi
    shard_load[$target]=$(( shard_load[$target] + cost ))
  done <<<"$placed"
  rm -f "$cost_file"
}

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
  # The shard count is set once the daemon cap is known: see below. It is a
  # measured count, not a knob: an unsupported value could skip shards or
  # break the arithmetic that places the journeys.
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

  daemon_cap="${NM_E2E_DAEMON_MAX:-4}"
  if [[ "$daemon_cap" =~ ^\+?[0-9]+$ ]]; then
    daemon_cap="${daemon_cap#+}"
    while [[ "$daemon_cap" == 0* && "${#daemon_cap}" -gt 1 ]]; do
      daemon_cap="${daemon_cap#0}"
    done
    if [[ "$daemon_cap" == 0 ]]; then
      daemon_cap=4
    fi
  else
    daemon_cap=4
  fi

  if [[ "$daemon_cap" == 0 || "$daemon_cap" == 1 ]]; then
    go test -tags=e2e -count=1 -timeout "${budget_seconds}s" ./internal/e2e/... ./internal/pipeline/steps/... &
    serial_pid=$!
    wait "$serial_pid"
    serial_status=$?
    exit "$serial_status"
  fi

  # The shard count is bounded by what the daemon cap leaves for the shards.
  # The terminal worker runs alongside them and takes one slot, and each
  # shard is a whole go test process that needs a slot of its own. A shard
  # that cannot get a slot does not wait - it fails, and the suite reports a
  # failure that has nothing to do with the code under test. So the shards
  # never outnumber the slots that are left.
  shard_count=$(( daemon_cap - 1 ))
  if (( shard_count < 1 )); then
    shard_count=1
  fi
  if (( shard_count > 3 )); then
    # More shards than this only adds contention on a small runner. Three
    # shards carry the measured e2e bucket - about 4000 seconds of work -
    # inside the 2400 second budget, and they are bounded by the cap.
    shard_count=3
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
  #
  # The two inventories are kept apart. The pipeline-step tests run in their
  # own worker and are never placed in a shard; the e2e journeys are the
  # ones the shards balance. Keeping them apart is what makes "every test
  # runs exactly once" true: a test that is both placed in a shard and run
  # by another worker would run twice.
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
    if [[ "$package" == "./internal/e2e/..." ]]; then
      all_tests="${all_tests}${listed}
"
    fi
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

  # The pipeline-step tests get their own worker. They are 1423 tests, and
  # they are FAST, but they are also the reason the daemon-slot-constrained
  # shards were long: a shard's -run pattern names tests, not packages, so
  # every shard ran every package's selected tests. The step tests never
  # take a daemon slot - only the e2e harness does - so a worker of their
  # own competes for nothing.
  #
  # Giving them their own worker also keeps them out of the shards'
  # discovery: a shard's pattern then names only journeys, and the step
  # tests are no longer counted twice.
  #
  # This worker runs FIRST and is awaited before the shards start. The
  # shards are the long pole of the suite, and every daemon slot the step
  # worker is not using is a slot a shard can have.
  go test -tags=e2e -count=1 -timeout "${remaining_seconds}s" ./internal/pipeline/steps/... &
  steps_pid=$!

  # The terminal-merge and retained-target journeys keep their own worker:
  # they are the two slowest live journeys, and sharing a worker with the
  # rest of the bucket is what made that worker the suite's long pole.
  #
  # This worker runs ALONGSIDE the shards, so it takes one of the daemon
  # cap's slots for as long as it runs. The shard count is bounded by what is
  # left of the cap, so a shard can always get a slot: a shard that cannot
  # get one does not wait - it fails, and the suite reports a failure that
  # has nothing to do with the code under test.
  go test -tags=e2e -count=1 -timeout "${remaining_seconds}s" \
    -run '^(TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$' ./internal/e2e/... &
  terminal_pid=$!

  # Everything else is split across shards. Each shard runs its own go test
  # process with its own timeout, so a slow journey in one shard cannot eat
  # the allowance of the others, and the bucket costs the time of its slowest
  # shard rather than the sum of all of them.
  #
  # The split balances by MEASURED TIME, not by test count. A single journey
  # can cost more than every other test in a shard together - the
  # retained-target journey alone costs more than ten minutes - so a
  # count-based split leaves one shard as the suite's long pole while the
  # rest idle. The measured cost of each journey lives in
  # scripts/e2e-journey-costs.txt, and the placement is the
  # longest-processing-time-first rule: the most expensive journey goes to
  # the least loaded shard, one at a time, and the rest follow in the same
  # order.
  #
  # The placement is a function of the measurements alone, so it is
  # deterministic: a journey does not move when an unrelated one is added,
  # and a failure reproduces in the same place.
  shard_tests=()
  shard_load=()
  shard_index=0
  while (( shard_index < shard_count )); do
    shard_tests[$shard_index]=""
    shard_load[$shard_index]=0
    shard_index=$((shard_index + 1))
  done

  # The measured cost of every discovered test, and the placement.
  place_shards

  # The step-test worker is awaited BEFORE the shards start. It takes no
  # daemon slot, but it does take a go test process and a CPU, and the
  # shards are the long pole of the suite: every resource the step worker
  # is using is a resource a shard is not. The step tests take about seven
  # minutes, and the shards need the whole budget.
  steps_status=0
  wait "$steps_pid" || steps_status=$?
  if (( steps_status != 0 )); then
    wait "$terminal_pid" || true
    exit "$steps_status"
  fi

  shard_pids=()
  shard_statuses=()
  shard_index=0
  while (( shard_index < shard_count )); do
    if [[ -z "${shard_tests[$shard_index]}" ]]; then
      shard_index=$((shard_index + 1))
      continue
    fi
    shard_pattern="$(printf '%s\n' "${shard_tests[$shard_index]}" | paste -sd'|' -)"
    go test -tags=e2e -count=1 -timeout "${remaining_seconds}s" \
      -run "^(${shard_pattern})$" \
      -skip '^(TestUserJourney|TestTargetAdvanceTerminalMergeLiveJourney|TestRetainedTargetContinuityLiveJourney)$' \
      ./internal/e2e/... &
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
  if (( steps_status != 0 )); then
    exit "$steps_status"
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
