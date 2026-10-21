#!/usr/bin/env bash
# Race suite wrapper.
#
# The race suite is the CI job's wall clock. Run as ONE go test process, its
# cost is the SUM of every package's race time, which outgrew the job's
# bound: the suite was killed at 45 minutes with internal/pipeline/steps and
# fifteen more packages never started, so the job could not report a failing
# test - it could not report a PASSING one either.
#
# The suite is split across parallel shards instead. Each shard is its own go
# test process with its own per-package timeout, so the job costs the time of
# its SLOWEST shard rather than the sum of all of them.
#
# The split is by PACKAGE, not by test name, and that is deliberate. A
# package's race time is the cost of its slowest test, so splitting a
# package's tests across shards would run the same expensive test in more
# than one shard and save nothing. The packages are therefore assigned whole,
# and the assignment balances by MEASURED TIME rather than by package count:
# the packages below are the measured long poles - each one costs more than
# the rest of a shard together - so they are handed out one per shard
# explicitly, and the remaining packages are spread by a checksum of the
# package's own path.
#
# The shard count is deliberately small: each shard is a whole go test
# process, and the runner is a small shared VM, so more shards than the
# machine can serve only adds contention. It is a measured count, not a
# knob: an unsupported value could skip shards or break the arithmetic that
# places the long poles.
set -u

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT" || exit 1

if [[ "$#" -ne 0 ]]; then
  go test "$@"
  exit $?
fi

# The per-package timeout. A package that exceeds it is a real failure and
# must be reported as one, not swallowed by the job's wall clock.
package_timeout_seconds=1500

# The shards.
shard_count=3

# The measured long poles of the suite, one per shard. Each of these costs
# more than the rest of a shard together, so two of them in one shard would
# make that shard the suite's long pole.
#
# The identities are the ones `go list` emits - the full import path - so
# every package in the inventory is named the same way. A relative path here
# would never match the inventory, and the long pole would then be assigned
# by the checksum as well: the expensive package would run twice and the
# balance would be defeated.
slow_packages=(
  github.com/kunchenguid/no-mistakes/internal/daemon
  github.com/kunchenguid/no-mistakes/internal/pipeline/steps
  github.com/kunchenguid/no-mistakes/internal/pipeline
)

# Every package of the suite. The list is the suite's inventory: a package
# that is not in a shard does not run, so the inventory is built ONCE and
# every package lands in exactly one shard.
all_packages="$(go list ./... 2>/dev/null | grep -v '/internal/e2e\($\|/\)')"
if [[ -z "$all_packages" ]]; then
  echo "race suite could not list the packages" >&2
  exit 1
fi

# Place the measured long poles, round robin over the shards, so a shard
# count that does not match the number of long poles still places each one of
# them exactly once.
shard_slow=()
slow_index=0
while (( slow_index < ${#slow_packages[@]} )); do
  target=$((slow_index % shard_count))
  if [[ -z "${shard_slow[$target]-}" ]]; then
    shard_slow[$target]="${slow_packages[$slow_index]}"
  else
    shard_slow[$target]="${shard_slow[$target]}
${slow_packages[$slow_index]}"
  fi
  slow_index=$((slow_index + 1))
done

# The rest of the packages are spread by a checksum of the package's own
# path. The path is stable across runs, so a package always lands in the
# same shard and a failure reproduces in the same place.
shard_rest=()
shard_index=0
while (( shard_index < shard_count )); do
  shard_rest[$shard_index]=""
  shard_index=$((shard_index + 1))
done

for package in $all_packages; do
  skip=0
  for slow in "${slow_packages[@]}"; do
    if [[ "$package" == "$slow" || "$package" == "${slow}/..." ]]; then
      skip=1
      break
    fi
  done
  if (( skip == 1 )); then
    continue
  fi
  target="$(printf '%s\n' "$package" | awk -v count="$shard_count" '
    {
      name = $0
      checksum = 0
      for (i = 1; i <= length(name); i++) {
        checksum = (checksum * 31 + index("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", substr(name, i, 1))) % 1000003
      }
      print checksum % count
    }')"
  if [[ -z "${shard_rest[$target]}" ]]; then
    shard_rest[$target]="$package"
  else
    shard_rest[$target]="${shard_rest[$target]} $package"
  fi
done

shard_pids=()
shard_statuses=()
shard_index=0
while (( shard_index < shard_count )); do
  packages="${shard_slow[$shard_index]-} ${shard_rest[$shard_index]-}"
  packages="$(printf '%s\n' $packages | grep -v '^$' | tr '\n' ' ')"
  if [[ -z "${packages// /}" ]]; then
    shard_index=$((shard_index + 1))
    continue
  fi
  # shellcheck disable=SC2086
  go test -race -count=1 -timeout "${package_timeout_seconds}s" $packages &
  shard_pids+=("$!")
  shard_statuses+=(0)
  shard_index=$((shard_index + 1))
done

shard_index=0
while (( shard_index < ${#shard_pids[@]} )); do
  wait "${shard_pids[$shard_index]}" || shard_statuses[$shard_index]=$?
  shard_index=$((shard_index + 1))
done
shard_index=0
while (( shard_index < ${#shard_statuses[@]} )); do
  if (( shard_statuses[$shard_index] != 0 )); then
    exit "${shard_statuses[$shard_index]}"
  fi
  shard_index=$((shard_index + 1))
done
exit 0
