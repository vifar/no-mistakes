---
title: Fork CI on a self-hosted runner
description: Self-hosted-only workflow execution and the required execution policy.
---

This fork runs every job of every workflow (`CI`, `docs`,
`Guard generated files`, `Nix flake`, and `Require no-mistakes`) on a single
repository-scoped self-hosted Linux runner, hosted in a dedicated virtual
machine. Nothing runs on a GitHub-hosted runner. Every job requires the
runner's full label set (`self-hosted`, `Linux`, `X64`, plus the fork's
dedicated runner labels pinned in `workflow_self_hosted_test.go`). There is no hosted fallback
when this runner is offline; jobs queue until it returns. It is a single
runner, so jobs from all workflows run one at a time.

Every job also carries the same-repository guard
`github.event_name != 'pull_request' || github.event.pull_request.head.repo.full_name == github.repository`,
so code from an external fork pull request never reaches the runner. The
release and channel-publishing workflows were removed from this fork; it
does not publish CLI releases or update channels from Actions. The docs workflow
still deploys GitHub Pages from `main`. The self-hosted workflow test
(`workflow_self_hosted_test.go`) walks every workflow file and rejects incorrect runner labels, missing job
timeouts, unsafe fork admission, or persisted checkout credentials.

External fork PRs skip the required check in this solo-use repository; approving
a workflow run does not bypass the job guard. The workflow generator for users'
repositories (`internal/cli/ci_workflow.go`) is unchanged.

CI runs on pull requests from branches in this repository, pushes to `main`,
and manual dispatches. It checks formatting, the generated skill, and vet;
runs the full Go race suite; builds the Linux CLI; cross-compiles the other
desktop targets; and runs end-to-end journeys through `make e2e`. Go 1.27.1
avoids the race-enabled subprocess issue documented for Go 1.26.0–1.26.4.
The Git for Windows receive-hook test (`TestReceiveHooksGitForWindows`, `e2e`
tag) is not run by this fork's CI: no hosted Windows runner is used, and
cross-compilation remains the only Windows and macOS check. It does not prove
runtime behavior there.

The race suite runs through `scripts/race.sh` in three parallel shards,
with whole packages assigned exactly once. The measured long poles
(`internal/daemon`, `internal/pipeline/steps`, `internal/pipeline`) each get
their own shard; remaining packages are assigned by a checksum of their full
import path. That assignment stays stable when unrelated packages are added.
Package discovery must succeed before any shard starts. Each shard uses
`-race -count=1` and a 25-minute timeout per package; the CI job has a
60-minute wall-clock bound, including builds. The wrapper contracts are
covered by `internal/e2edaemon/race_wrapper_test.go`.

Every CI job checks out the exact PR head (or event SHA outside a PR) without
persisting checkout credentials.

The runner needs Git, Make, Python 3 (for the `Require no-mistakes` action), a C compiler with libc development headers (for
the race detector), the `sqlite3` CLI used by the Pi-profile journeys, and
multi-user Nix with an active `nix-daemon` usable by the runner account. The `Nix flake`
workflow adds `/nix/var/nix/profiles/default/bin` to `GITHUB_PATH` by default.
Administrators install these dependencies; the runner account does not gain
sudo access. The default end-to-end suite shares a 40-minute budget inside
the 90-minute job bound. With at least two daemon slots, the user-journey
matrix runs in two isolated Go test processes: Claude and Codex in one, all
other agents in the other. The terminal-merge and retained-target continuity
journey tables then share one Go test process alongside a separate
pipeline-step worker. Before starting those workers, the wrapper checks test
discovery in both package trees and fails if either discovery fails or returns
no tests. It waits for the pipeline-step worker to succeed before starting
shards containing the remaining e2e tests.

Shard placement uses the estimated costs in `scripts/e2e-journey-costs.txt`:
the most expensive remaining journey goes to the least loaded shard, with
unlisted journeys using the file's default cost. Placement is deterministic
for a fixed test inventory, cost table, and shard count. Adding a journey or
changing a cost can move other journeys between shards. The matrix, terminal
journeys, and pipeline-step tests are excluded from shard placement so each
test runs in its assigned worker only.

The pipeline-step worker uses CPU and build resources but does not use a
temporary-daemon slot. Waiting for it before starting the e2e shards reduces
resource contention. The terminal worker retains the allowance calculated
after the matrix and discovery; the shards receive a newly calculated
allowance after the step worker finishes and placement completes. An
exhausted allowance prevents launch. These allowances are passed as Go's
per-package test timeout; compilation and wrapper cleanup remain subject to
the CI job's wall-clock bound.

The daemon cap bounds how many temporary daemons may be live at once. The
wrapper defaults to five slots and uses at most four e2e shards, reserving
one slot for the terminal worker. A lower cap reduces the shard count; with
one slot, the complete suite runs in a single Go test process. Harness slot
acquisition waits for up to two minutes before failing. Each invocation
retains the shared temporary-daemon inventory and cleanup trap. Wrapper
contracts are covered by `internal/e2edaemon/wrapper_budget_test.go`.

## Required execution policy

This public fork must enforce `.github/ci-actor-policy.json` as an **active,
repository-wide** GitHub Actions policy before any persistent local runner
is registered. It allows only the repository owner and the existing official
`github-actions[bot]`, `dependabot[bot]`, and `release-please[bot]` identities.
There is no workflow-path restriction: the policy applies to every workflow.

A job-level same-repository guard is an additional check. It cannot replace
the policy, because a contributor could change another workflow's runner
labels in a pull request. Do not automatically approve or rerun external
contributor workflows. Owner-triggered execution of external code requires
explicit authorization and review of the code and workflow first.

An administrator can apply the policy through GitHub's repository Actions
policies API, using the JSON file as the request body. Read it back and verify
the active enforcement, complete actor list, and absence of path exclusions.
Keep the runner stopped if that policy is removed or weakened. Policy changes
are administrative changes; editing the JSON file alone does not apply them.

See [GitHub's workflow execution controls](https://docs.github.com/en/actions/how-tos/administer/control-workflow-execution).

## Runner ownership and recovery

The runner runs as a dedicated unprivileged account with a private home
directory and its own `actions-runner` workspace. This account has neither
sudo access nor Docker-group membership, and runs the runner as its own
system service. Other runners on the same virtual machine keep their separate
accounts, registrations, and services.

If CI queues, inspect this runner's status on the repository Actions runners
page and its service on the runner's virtual machine. Recheck the policy before
starting or re-registering it. Use GitHub's official runner distribution,
verify its published SHA-256 digest, and pass a temporary registration token
through a protected channel. Never save that bootstrap token in this repo.
Runner credentials remain restricted to the runner account.

The `docs`, `Guard generated files`, `Nix flake`, and `Require no-mistakes`
workflows run on the same runner. `actions/setup-node` downloads its own Node.
The Nix job does not install Nix: an administrator installs multi-user Nix on
the runner's virtual machine once (for example with the Determinate installer and `sudo`), and the
job fails with an actionable error if it is missing. None of these workflows change
publication destinations or install the CLI on a developer machine.

### Go cache: disabled on the self-hosted jobs

The `check`, `test` and `e2e` jobs set `cache: false` on `actions/setup-go`. The runner is persistent, so the Go module and build caches already stay on its local disk between jobs. The action's own cache only adds a post-job upload. On this runner that upload has hung in the `Post Run actions/setup-go` step after every real step had passed, until the job hit its `timeout-minutes` and GitHub reported the job as **cancelled**.

If a CI job shows `cancelled` while all of its test and build steps succeeded, check the post-job steps first. Rerun the failed job, rather than treating it as a test failure. Keep `cache: false` unless the runner becomes ephemeral; in that case, add a separately timed cache step instead of the action's built-in cache.

## Self-update destination

The [update command reference](/no-mistakes/reference/cli/#no-mistakes-update)
owns the fork destination, manifest requirements, and refusal behavior. The fork
must publish its own channel manifest and release assets before release updates
can succeed.
