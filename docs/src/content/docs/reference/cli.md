---
title: CLI Commands
description: Complete reference for all no-mistakes commands and flags.
---

## no-mistakes

Attach to the active pipeline run for the current branch when one exists. If none exists, bare `no-mistakes` can start the setup wizard to create a branch, commit changes, push through the gate, wait for the daemon to register the new run, and then attach. If the push succeeds but no run is registered, that wizard path now exits with an explicit error instead of silently falling through. By default this wizard path is interactive and only runs in a TTY session. In non-interactive contexts, bare `no-mistakes` falls back to showing the last 5 runs inline unless you pass `-y` or `--yes` to run the wizard and accept defaults automatically. When a TTY is available, `-y` keeps the wizard visible, shows a brief `waiting for run…` state after push, and auto-advances the default path; without a TTY it falls back to the headless path.

```sh
no-mistakes
no-mistakes --skip test,lint
```

| Flag          | Type     | Default | Description                                          |
| ------------- | -------- | ------- | ---------------------------------------------------- |
| `-y`, `--yes` | `bool`   | `false` | Run setup wizard and accept defaults automatically   |
| `--skip`      | `string` | (none)  | Comma-separated pipeline steps to skip for a new run |

Unlike `no-mistakes attach`, bare `no-mistakes` only auto-attaches to an active run on the current branch.
`--skip` only applies when bare `no-mistakes` starts a new pipeline run through the wizard; it does not skip a step on an already-active run.
The only valid `--skip` step names are `intent`, `rebase`, `review`, `test`, `document`, `lint`, `push`, `pr`, and `ci`. Repository gate names are refused.

## no-mistakes init

Initialize or refresh the gate for the current repository.

`init` requires an `origin` remote to identify the upstream repository: later pipeline steps push validated branches to the configured target and open pull requests against that upstream. If `origin` is missing, add it with `git remote add origin <url>`, replacing `<url>` with the upstream repository's URL, then re-run `init`.

```sh
no-mistakes init
no-mistakes init --fork-url git@github.com:you/my-repo.git
no-mistakes init --worktree-root ~/work/my-repo-runs
no-mistakes init --install-skill=false
```

| Flag              | Type     | Default | Description                                                                                      |
| ----------------- | -------- | ------- | ------------------------------------------------------------------------------------------------ |
| `--fork-url`      | `string` | (none)  | GitHub fork remote URL to push branches to while opening PRs against `origin`                  |
| `--worktree-root` | `string` | (none)  | Directory to create this repository's run worktrees in; prints the `worktree_roots` entry to add |
| `--install-skill` | `bool`   | `true`  | Install or refresh the agent skill at user level; set to `false` to leave user skills untouched |

Creates or refreshes a local bare repo, installs the managed pre-receive admission and post-receive notification hooks, best-effort isolates the gate repo's hook path from shared git config changes when Git supports `config --worktree`, adds or repairs the `no-mistakes` git remote, detects the default branch, records or updates the repo in SQLite, installs the `/no-mistakes` agent skill by default at user level into `~/.claude/skills/no-mistakes/SKILL.md` and `~/.agents/skills/no-mistakes/SKILL.md`, and ensures the daemon is running, installing the managed service when available and falling back to a detached daemon otherwise.
`init` writes no skill files into the repo; the user-level copies serve Claude Code (`~/.claude/skills`) and agents that use the vendor-neutral `~/.agents/skills` convention (Codex, OpenCode, Rovo Dev, Pi, and Omp) across all repos. Grok Build is a pipeline runner and does not consume this installed skill.
If the home `.claude` links to `.agents`, `.claude/skills` links to `.agents/skills`, or the reverse, `init` follows that layout and still makes the skill readable from both logical paths.
If the repo still contains a vendored skill copy written by an older no-mistakes version, `init` leaves it untouched and prints a notice that it is no longer needed and can be removed.
The gate advertises Git push-option support, so you can skip steps for one push with `git push -o no-mistakes.skip=test,lint no-mistakes <branch>`.

Managed receive hooks pin the canonical native home and the executable used during enrollment or explicit refresh. Ambient `NM_HOME` and a `no-mistakes` executable on `PATH` cannot redirect those helpers. If the pinned executable disappears, admission refuses the push; notification records a failure for an already accepted push.

For an isolated installation, use a separate native home, binary, and independent Git clone. Pass `init --install-skill=false` to leave user-level agent skills unchanged. A linked worktree shares its Git common directory and remotes with its parent checkout, so it is not an isolated enrollment target.

For GitHub fork contributions, keep `origin` pointed at the parent repository and pass `--fork-url` with your fork remote URL.
The Push step, rebase branch-sync, and CI repair publication use the fork, including when [`ci.revalidate_repairs`](/no-mistakes/reference/repo-config/#cirevalidate_repairs) sends a repair back through Push, while GitHub PR and CI commands stay scoped to the parent repository and create PRs with `--head <fork-owner>:<branch>`.
Fork routing currently requires both `origin` and `--fork-url` to be GitHub remotes with owner/repo paths.

Without `--fork-url`, `init` best-effort detects the opposite (and common) fork layout `gh repo fork --clone` leaves behind - `origin` is your own fork and a separate `upstream` remote names the parent - and refuses with guidance instead of silently treating your fork as the parent, which would open PRs and bind attestations inside your fork rather than the project you're contributing to. Detection only fires with positive proof (a GitHub API answer confirming `origin` is a fork of exactly what `upstream` names) and fails open whenever it cannot get that proof - no `upstream` remote, either remote off GitHub, or `gh` unavailable/unauthenticated/offline - so it never blocks a repo that merely happens to have an unrelated `upstream` remote. Fix a refusal by pointing `origin` back at the parent and passing your fork as `--fork-url`, per [CONTRIBUTING.md](https://github.com/kunchenguid/no-mistakes/blob/main/CONTRIBUTING.md).

`--worktree-root` is for directory-scoped toolchain configuration (mise, direnv), which resolves by path ancestry and so never reaches a run worktree under `NM_HOME`.
The flag resolves the directory, then prints the [`worktree_roots`](/no-mistakes/reference/global-config/#worktree_roots) entry to add to `~/.no-mistakes/config.yaml`; the global config is hand-maintained, so `init` never rewrites it for you.
When the file already has a `worktree_roots:` block, `init` prints just the entry line to add under it - a second `worktree_roots:` key would make the config unparseable and stop the daemon.
Runs are created at `<dir>/<run id>` once the entry is in place; no-mistakes only ever touches the directories its own run records name, and everything else in that directory is left alone.
`init` rejects the directories the daemon would refuse to start on, so the entry it prints is always one you can paste: a directory inside `NM_HOME`, inside the repository being initialized or any other gated checkout, already used by another checkout (it names that checkout), or that exists as a non-directory.

Two refusals apply to every `init`, with or without the flag.
It refuses to register a checkout that contains a directory an existing [`worktree_roots`](/no-mistakes/reference/global-config/#worktree_roots) entry points at, naming that entry, because registering it is what would make the placement unusable and stop the daemon; place the checkout elsewhere or repoint the entry first.
It also refuses to register anything while `~/.no-mistakes/config.yaml` does not load, naming the fault, because the daemon refuses to start on that same config.

Re-running `init` on an already-initialized repo succeeds and reports `Gate already initialized (refreshed)`.
It refreshes managed gate wiring, origin/default-branch metadata, hook-path isolation, and, by default, the installed agent skill, overwriting any stale `SKILL.md` content from an older binary.
When a fork URL is already recorded, re-running `init` without `--fork-url` preserves it.
Passing `--fork-url` again replaces the stored fork URL after validation.
If you rename or move an initialized working directory and the old path no longer exists, re-running `init` from the new path reattaches the existing gate, preserves the repo ID and run history, and updates the stored working path.
If you copy an initialized working directory while the original still exists, the copy is treated as a separate repo and gets a fresh gate.
Fresh init rolls back gate setup when a required gate or daemon step fails; refresh does not eject a pre-existing gate if daemon startup fails.
Skill installation is best-effort: if the skill write fails, init reports it and leaves the working gate in place.
With `--install-skill=false`, fresh initialization and refresh skip user-level skill installation entirely and report it as disabled. Existing user skills remain untouched. The flag does not change gate setup or daemon startup, and must be passed on each invocation that should skip installation.

## no-mistakes ci-workflow

Generate `.github/workflows/ci.yml` from the repository's `.no-mistakes.yaml` commands, so GitHub Actions registers real checks that the gate's CI step can monitor.

```sh
no-mistakes ci-workflow
no-mistakes ci-workflow --force
```

| Flag            | Type   | Default | Description                      |
| --------------- | ------ | ------- | -------------------------------- |
| `-f`, `--force` | `bool` | `false` | Overwrite existing workflow file |

Run it from anywhere inside a repository with a `.no-mistakes.yaml`; the config is read from and the workflow written at the git toplevel, and `commands.test` must be configured or the command errors.
The generated workflow runs the configured test command (and lint command when `commands.lint` is set; with an empty `commands.lint` it emits a test-only workflow, since lint runs as the combined document+lint agent pass) on push to the repository's default branch, resolved from the `origin` remote and falling back to `main`, and on all pull requests.
Commands are inserted verbatim into YAML block scalars, with multi-line commands indented to stay inside the scalar.
The template is Go-focused (it uses `actions/setup-go` with `go-version-file: go.mod`); non-Go repos can adapt the generated file.
An existing `.github/workflows/ci.yml` is never overwritten without `--force`, overwrites are atomic, and the command refuses to write through a symlinked `.github`, `workflows`, or `ci.yml`.
Commit and push the generated file to enable the checks.

## no-mistakes axi

Agent eXperience Interface for non-interactive agents.
Most agent workflows use the installed `/no-mistakes` skill, which drives this command surface underneath.
It prints TOON to stdout, except [`review-receipt`](#no-mistakes-axi-review-receipt) and [`ci-handoff`](#no-mistakes-axi-ci-handoff), which emit JSON on success. It prints progress to stderr and uses structured stdout errors with exit code `1` for operational failures and `2` for bad usage; `ci-handoff` uses JSON errors and exit code `1`.
At the TOON output boundary, unsupported C0 control bytes are rendered as visible `\xNN` escapes while tabs, carriage returns, newlines, printable Unicode, and the underlying durable logs remain unchanged.
If TOON encoding still fails, AXI prints a structured error instead of returning successful empty stdout.
The calling agent drives AXI approval gates but does not replace the configured pipeline agent that performs validation.

```sh
no-mistakes axi
```

With no subcommand, shows the executable path, description, repo, current branch, daemon state, recent runs, and next-step help, including a pointer to `no-mistakes axi run --help` and the installed `/no-mistakes` skill for full driving guidance.
When the daemon is reachable, a `slots` line reports the run-concurrency cap and usage (for example `slots: "2/3 in use, 4 queued"`; `N in use (no cap)` for an unlimited cap). The queued count includes resumed runs waiting for a slot. A `parked (slot released)` count includes live runs that yielded at a gate or during CI monitoring. `axi status` shows the same line.
A new run waiting for a slot has `status: queued`, `queue_position: N`, and `queue_length: M`. A resumed run keeps `status: running` and adds `waiting_for_slot: true` with its position. After an accepted gate response or reconciliation claim, its step's activity reads `already answered; waiting for a run slot` until dispatch, and the run includes `answered: <step>: answered, waiting for a run slot` with no `gate:` or `awaiting_agent` signal. A recovered gate can also have `waiting_for_slot: true` before it is answered; it still shows its unanswered gate. Home and status use the daemon's live positions, including resumed waiters, when reachable. Otherwise, positions come from the database's queued new runs; live slot usage and resumed waiters are unavailable. See [`daemon`](/no-mistakes/reference/global-config/#daemon) for admission rules.
When the current branch has an active run, that run appears as `active_run` with any approval gate and help for `axi respond` when it is parked or `axi status` when it is still running.
If an active run object has an unanswered decision gate, it includes `awaiting_agent: parked <duration>` immediately after `status`.
That field is observability only; the `gate:` object still tells the agent which response to send.
If a step is actively `running` or `fixing`, the run object can also include an `active_steps` table with step-scoped `active_for`, current-round `round_active_for`, `last_activity`, native `agent_pid` when one is currently running, and the current execution or fix round.
When only another branch has an active run, that run appears as `other_branch_active_run`; the help tells agents to leave it alone and start validation for the current branch.
AXI help and outputs always repeat the preserve-prior-gate-progress contract: after a gate round has already produced fix commits, additional fixes belong on the same branch.
When a relevant `branch_sync` object is present, they also include version-matched synchronization guidance to follow before a post-pipeline local commit or fresh run.
Agents must not abort-and-restart, reset, replace the branch, or improvise Git recovery in a way that drops prior gate-fix commits.
A fresh run re-validates the current branch state, so already-resolved findings do not re-surface.

## no-mistakes axi run

Start or reattach to validation for the current branch, blocking until the first approval gate, CI-ready decision point, or final outcome.
An active run on another branch does not block starting validation for the current branch.

```sh
no-mistakes axi run --intent "the user's goal"
no-mistakes axi run --intent "the user's goal" --skip test,lint
no-mistakes axi run --intent "the user's goal" --yes
no-mistakes axi run --intent "the user's goal" --base-branch epic/foo
no-mistakes axi run --intent "the user's goal" --skip=push,pr,ci --external-ci-owner=<owner>
no-mistakes axi run --intent "the user's goal" --no-publish-intent
no-mistakes axi run --intent "the user's goal" --closes 95 --closes owner/repo#12
```

| Flag            | Type     | Default | Description                                                                                          |
| --------------- | -------- | ------- | ---------------------------------------------------------------------------------------------------- |
| `--intent`      | `string` | (none)  | What the user set out to accomplish; `-` reads stdin to EOF; exclusive with `--intent-file` |
| `--intent-file` | `string` | (none)  | Read intent from a file relative to the caller's working directory; exclusive with `--intent` |
| `--verification-plan` | `string` | (none) | Path to a nonempty UTF-8 verification plan, at most 64 KiB (65,536 bytes), captured as separate evidence for a new run only |
| `-y`, `--yes`   | `bool`   | `false` | Auto-resolve eligible gates until a decision point or outcome                                       |
| `--skip`        | `string` | (none)  | Comma-separated pipeline steps to skip                                                               |
| `--base-branch` | `string` | (none)  | Integration branch for this run only; overrides [`pr.base_branch`](/no-mistakes/reference/repo-config/#prbase_branch) |
| `--shipping-mode` | `string` | (none) | Select `full` or `immutable-source`; see [strict shipping](#strict-shipping-and-review-receipts) |
| `--draft` | `bool` | (unset) | Pin the GitHub draft choice; requires `--shipping-mode`, supports explicit `false` |
| `--external-ci-owner` | `string` | (none) | For ordinary runs, hand off an explicit CI claim; requires exactly `--skip=push,pr,ci`. Unavailable in strict shipping. |
| `--no-publish-intent` | `bool` | `false` | Keep the generated `## Intent` section out of the PR body for this run; tighten-only, see below |
| `--closes` | `string`, repeatable | (none) | Issue the PR fully resolves (`95`, `owner/repo#95`, or a nested GitLab project path); see [Closing issues](#closing-issues) |
| `--model` | `string` | (none) | Pi provider/model ID for an immutable [per-run profile](/no-mistakes/reference/global-config/#per-run-pi-profiles) |
| `--effort` | `string` | (none) | Pi reasoning effort for that profile; omitted fields inherit `agent_config.pi` |
| `--wait`        | `duration` | `8m`    | Maximum time for active-run lookup and run driving before the caller must reattach |
| `--launch-nonce` | `string` | (none) | Non-secret correlation identifier for a durable pre-drive receipt; requires `--validation-generation` |
| `--validation-generation` | `string` | (none) | Caller-selected validation generation bound to `--launch-nonce`; requires that flag |

Explicit intent is the user's goal or request, not a description of the diff.
no-mistakes uses the supplied text instead of transcript inference; see [Intent input](#intent-input) for transports and whitespace handling.
Err on the side of completeness: include the goal, important decisions and tradeoffs, constraints or approaches ruled in or out, and explicit requests that might otherwise look surprising in the diff.
When starting an ordinary or `full` run, `axi run` refuses the default branch and uncommitted working trees with actionable errors instead of auto-branching or auto-committing. [`immutable-source`](#strict-shipping-and-review-receipts) permits a default-branch caller but still requires a clean checkout.
Ordinary reattachment to an in-flight run does not require intent input and never replaces that run's intent, even if new text is supplied. [Strict launch receipts](#strict-launch-receipts) require the original intent bytes on every retry, using any of the input transports below.

### Intent input

A new run requires exactly one of `--intent TEXT`, `--intent-file PATH`, or `--intent -` (stdin until EOF). Explicit empty or whitespace-only input is rejected, including on reattachment, and never falls back to transcript inference. `--intent` and `--intent-file` conflict even when either flag's value is empty. Missing/unreadable files and stdin read errors are rejected before starting a run or taking branch custody. No flag means stdin is not read.

All three inputs must be valid UTF-8 and at most **49,122 bytes**, including whitespace. This ceiling comes from the existing single Git push-option transport: its 65,516-byte packet payload must hold the option prefix and base64-encoded intent. Oversized or malformed input is rejected before run resources open, never truncated or repaired. `--intent-file` requires a regular file (a symlink to one is allowed); directories, devices, and named pipes are rejected. Use `--intent -` for piped input: it reads until EOF, or rejects as soon as the size limit is exceeded, with no input timeout.

File and stdin content reaches the existing run request unchanged, including quotes, backticks, dollar signs, Unicode, and multiline/trailing newline content. All three transports then follow the same existing run semantics: ordinary explicit intent is stored with leading/trailing whitespace removed; strict launches and runs with a verification-plan attachment preserve those bytes. This is not a universal persisted-byte guarantee. File paths are literal (`--intent-file -` names a file called `-`); only `--intent -` selects stdin. To supply the literal intent `-`, use a file or stdin.

```sh
no-mistakes axi run --intent-file ./intent.md
no-mistakes axi run --intent - < ./intent.md
# INTENT already contains the prose; quoting the expansion preserves it.
printf '%s' "$INTENT" | no-mistakes axi run --intent -
```

The clean-worktree preflight still applies: keep an untracked intent file outside the worktree or ignore it.

For programmatic callers, prefer a file or write the text to the process's stdin; an argument-array API without a shell also safely supports `--intent TEXT`. Avoid interpolating free-form prose into shell command source: substitutions inside double quotes (such as backticks and `$()`) run in the **caller's shell**, before no-mistakes sees the argument. File/stdin input is not shell-evaluated by no-mistakes, but does not make unrelated unsafe shell commands safe. These transports do not change intent storage, prompt use, or publication policy; they are not a privacy control. Downstream transport limits still apply.

These options apply to `axi run`; the separate `no-mistakes rerun` command retains its existing inheritance and explicit-string override semantics.

### Verification plan attachment

```sh
no-mistakes axi run --intent "the user's goal, unchanged" --verification-plan /path/to/verification-plan.txt
```

The optional plan is author-supplied evidence, **not user intent or higher-priority instructions**. With this flag, the exact intent bytes are preserved separately, as described in [Intent input](#intent-input). Before pushing to the gate or taking branch custody, the daemon reads the source once and rejects missing, unreadable, nonregular, empty/whitespace-only, or non-UTF-8 files. Plans exceeding 64 KiB (65,536 bytes) are rejected, never truncated; accepted bytes are preserved unchanged. The read is bounded to 65,537 bytes to detect oversized input. Relative paths resolve from the caller's working directory. An older daemon that cannot capture this input is refused before the push.

The capture is bound to the repository, branch, and submitted commit. If HEAD advances during ordinary launch preparation and no longer matches the capture, launch is refused before changing the gate refs; retry the launch to capture the plan for the new commit.

The captured bytes live privately at `<NM_HOME>/run-inputs/<run-id>/verification-plan.txt`, outside the code worktree and the publishable Test-evidence directory. The run pins the snapshot location, SHA-256 digest, absolute source path and capture time. `axi status` reports these under `run.verification_plan` (`path`, `sha256`, `source_path`, `captured_at`, Unix seconds); a run without an attachment reports `verification_plan: none`. The IPC run representation uses JSON `null` for absence. The file is local input evidence, not automatically published on the PR.

Review and Test, including their fix turns, receive the same digest-checked snapshot as labeled evidence. Editing or deleting the original source cannot change it; a missing or altered snapshot fails closed when consumed. Reattach with `no-mistakes axi run` **without** `--verification-plan`; providing the flag for an existing run is refused, including strict-launch replay. A new run without the flag does not inherit a prior run's plan. Capture time records when no-mistakes read the file, not proof that its author wrote it before implementation.

Only attached runs receive plan-aware guidance. Review and Test assess the proposed scenarios and independent expected results against actual evidence; the attachment does not direct Review to execute verification. Test receives the execution guidance: when no existing check drives a scenario, perform repeatable product verification with a retained artifact, or name the missing capability and how to provide it and mark the scenario untested. Both steps must follow repository testing rules before changing permanent tests; an attached plan alone is not a reason to add tests. A missing-test finding must identify the observable failure, why existing checks and product evidence do not cover it, and the independent expected result. Runs without a plan retain their existing Review and Test guidance.

### Other run options

`--base-branch` is persisted on the run and honored after resume; the [Pipeline Steps scope rules](/no-mistakes/reference/pipeline-steps/) own its integration and change-scoping behavior.
Reattaching with a `--base-branch` that differs from the active run's stored target is refused rather than silently discarded; omit the flag to reattach, or abort the active run first.
The [`pr.base_branch` reference](/no-mistakes/reference/repo-config/#prbase_branch) owns target precedence and the one-time retarget rules. `--external-ci-owner` is explicit per run and persisted for recovery; a new run or rerun does not inherit it. To reattach to an active run with external CI ownership, supply the same explicit owner and required skip set; omitting the owner or supplying a conflicting owner is refused. It does not turn a skipped CI step into a passing check. When Review has pending CI support and Push, PR, and CI are explicitly skipped, the outcome is `pending-external-ci`. Read the typed handoff below and let the named external owner prove those claims before merge. Without that owner, pending claims block completion.
`--no-publish-intent` is likewise persisted on the run, and reattaching with it against an active run started without it is refused rather than silently discarded; omit the flag to reattach, or abort the active run first.
Before starting a run that may omit the section (this flag set, the global `intent.publish_intent` default `false`, or a global config that cannot be read), `axi run` probes the running daemon for the capability and refuses to start anything when that daemon is too old to honor it (an older daemon would silently drop the field, never read the global default, and publish); restart the daemon with the current binary. Only a run that cannot omit (flag unset, global default `true`) may reuse an older daemon. `rerun` always probes, because it inherits omission from the selected prior run and only the daemon knows that selection.
Under the flag the PR-drafting turns receive no intent text at all and draft from the diff and commit messages only; every other step prompt keeps the full intent.
The same omit-to-reattach rule applies to `--model`/`--effort` against an active run's [pinned Pi profile](/no-mistakes/reference/global-config/#per-run-pi-profiles); a different selection cannot change that pin.

### Closing issues

`--closes` declares an issue the PR fully resolves, so merging the PR closes it through the forge's native closing keywords. Repeat it for each issue. A value is a same-repository issue number (`--closes 95`) or a project-qualified reference (`--closes owner/repo#95`, or `--closes group/subgroup/repo#95` for a GitLab project nested under subgroups); anything else, including `#95`, is rejected before a run starts. References are deduplicated case-insensitively and rendered in a deterministic order, one `Closes` line each, in the PR body's `## Issues` section (see the [PR step](/no-mistakes/reference/pipeline-steps/#pr)).

`--closes` is the only way to request closure. Without it, no-mistakes never adds or infers a closing reference from the intent, commit messages, branch name, or linked issues: a PR may be partial work, so use ordinary references in your own text for that. The [PR step](/no-mistakes/reference/pipeline-steps/#pr) documents generated-text closure protection, author-preserved text, and the limits for custom GitLab patterns, PR titles, and commit messages.

The references are persisted on the run and survive daemon restarts, fix rounds, rebases, and that run's PR-body refreshes; `no-mistakes rerun` inherits them, and its own `--closes` adds to them. They can also be passed on a plain gate push as repeated `-o no-mistakes.closes=<ref>` push options; a gate push without them starts a run with none, and its PR-body refresh drops the `## Issues` section. Reattaching with `--closes` adds references to the active run until its PR body has been composed; after that the request is refused with an explicit error rather than reported as applied. Before starting a run with `--closes`, `axi run` refuses a running daemon too old to honor it, and rejects `--closes` combined with `--skip pr` as a usage error; a run whose PR step is skipped any other way fails at that step instead (see the [PR step](/no-mistakes/reference/pipeline-steps/#pr)).

`--closes` is supported on providers that close issues from the pull request body: GitHub and GitLab. On another forge the PR step fails rather than publish a PR that silently closes nothing; a GitLab subgroup reference on a non-GitLab provider is refused the same way, since that forge would read it as a different repository's reference. The forge closes the issue only when the PR merges into the repository's default branch (and, on GitLab, unless automatic issue closing is disabled for the project); a PR merged into another branch does not close it.
Ordinary reattachment accepts either the run's immutable submitted head or its current pipeline head, so pipeline-created fix commits do not detach an unchanged submitting worktree.
When neither identity matches, `axi run` keeps the fresh-run path but refuses a gate push while `branch_sync` says the pipeline still owns the branch.
That refusal returns the complete structured state and its `continue_active_run` or `recover_custody` next action instead of a raw Git non-fast-forward.
A fresh run also reconciles a private gate mirror left stranded by a terminal run before pushing: when this repository's own run records prove the mirror head superseded, the mirror is archived and replaced so the ordinary push proceeds; otherwise `axi run` exits with a structured `error:` naming the mirror head, the head being submitted, and how many commits the mirror holds that the submission does not contain, plus a `help:` carrying the operator step that is reachable in that exact state. The [private mirror reconciliation contract](/no-mistakes/concepts/gate-model/#private-mirror-reconciliation) owns the proof conditions and the refusal.
Reattaching to an in-flight run can proceed while the daemon is already running even if the global config file has become invalid, but starting a fresh run still requires valid global config.
Starting a fresh run also requires a runnable effective pipeline agent.
If the configured native agent or ACP runner is unavailable, the run fails before any pipeline step starts instead of reporting command-only validation as a passed gate.
With `--yes`, `axi run` treats both `action: auto-fix` and `action: ask-user` findings as standing consent for the pipeline to fix them by selecting every finding, then accepts the resulting fix review.
Gates with no findings or only `action: no-op` findings are approved as-is, and each step is fixed at most once so unresolved findings do not loop forever.
The [`protected_paths` refusal rules](/no-mistakes/reference/repo-config/#protected_paths) are an exception to this automatic handling.
So is an open [review question](/no-mistakes/concepts/review-conversation/), which is settled by an answer rather than by a verdict, and a review question history that cannot be read end to end: `--yes` stands aside at both instead of selecting findings.
So is a Test budget-cut gate that reports `test-agent-unvalidated-work`: approval is refused there, so `--yes` stops at it and leaves the choice between `--action fix` and `no-mistakes axi abort` to the operator (see [`test_agent_timeout`](/no-mistakes/reference/global-config/#test_agent_timeout)).
Without `--yes`, an agent driving `axi run` should stop when a gate contains `action: ask-user` findings and relay each finding's ID, file, and full description to the user before responding.
Review gates include a `note` field reminding agents that `auto_fix.review` defaults to `0`, so blocking and ask-user review findings park for a decision unless configuration explicitly opts back into review auto-fix.
Long-running `axi run` calls are working, not stalled; if one returns a `gate:`, read that output and answer it with `axi respond`.
When the daemon is at its `daemon.max_concurrent_runs` limit, the launch is recorded as a `queued` run and returns at once. Without an explicit `--wait`, `axi run` prints `queued (position N of M)` on stderr and returns success (exit 0) with the run object and `outcome: queued`; the run starts on its own when a slot frees. Passing `--wait` explicitly keeps driving through the queue, printing each position change, and never errors merely because the run is still queued. `axi abort --run <id>` cancels a queued run without ever starting it.
`--wait` defaults to 8m so an agent harness with a 10-minute tool cap gets a structured error instead of an unbounded hang. It bounds the active-run lookup, event-subscription acknowledgement, and subsequent run driving. Elapsed wait is not a pipeline failure and does not mean the daemon is dead: run `no-mistakes axi status` and reattach with `axi run`. A timed-out `get_active_run` or `get_run` state read, before or during run driving, is retried only after a successful health probe. That probe allows up to 5 seconds for the health reply, capped by the caller's remaining deadline. Connection failures, RPC errors, an unhealthy status, or no health reply within that bound still stop the driver; genuine state-read RPC errors are not retried.
Backgrounding a call is fine for an agent harness, but the run never advances past a gate on its own.
When the CI step is still monitoring an open PR and checks are green - or the trusted default-branch config declares [`no_ci: true`](/no-mistakes/reference/repo-config/#no_ci) with no registered checks - `axi run` exits successfully with `outcome: checks-passed` instead of waiting for a human merge. A generic empty check list without that declaration is not ready.
Treat that as the agent stopping point: ask the user to review and merge the PR from the `help` line.
For ordinary runs, when the live target commit advances with green checks and no merge conflict, CI clears readiness and keeps monitoring, withholding checks-passed.
It may wait until ci_timeout, or indefinitely when unlimited.
When CI returns, including with findings, the boundary guard rejects the changed comparison and terminates the run.
The monitor does not automatically repair or revalidate that changed comparison.
After termination, restart from the current target and head; never hand-rebase.
After the run stops, see [`no-mistakes rerun`](#no-mistakes-rerun) for restart and custody conditions.
Reattaching with `axi run` while the monitor is still active leaves HEAD unchanged and does not refresh the target comparison.
Successful outcomes (`checks-passed`, `passed`, `passed-with-override`, and `passed-with-skips`) also carry `help` instructions telling the agent to summarize the run.
`passed-with-override` is a completed run with an explicitly approved Test exception, an [accepted Test gap](/no-mistakes/reference/pipeline-steps/#accepted-test-gaps), or a CI approval over still-failing checks.
It stays a success but is distinct from a clean `passed`.
A Test exception is an approval past a failing configured `commands.test`, a `no-go` verdict, an `inconclusive` verdict, or a [`test_agent_timeout`](/no-mistakes/reference/global-config/#test_agent_timeout) budget cut; approving a `no-surface` park records its reason but completes as `passed`.
`run.test_override_reason` preserves the Test exception explanation in drive and status output, including at the `checks-passed` stopping point; CI overrides retain their separate reason.
Report those exceptions rather than describing Test as clean.
`passed-with-skips` describes a completed run with an automatic PR or CI skip, such as CI having no PR URL or an adapter recording provider unavailability. Ordinary guarded network runs instead fail when provider availability cannot be established before Rebase or at a later comparison boundary; an adapter skip does not bypass that check.
It retains exit code 0: missing verification is not a failing code verdict.
`run.automatic_skips` names each affected step and cause, and `run.head_sha` gives the full recorded head in both drive output and `axi status`.
Report that missing evidence; this outcome does not establish CI readiness or a merge.
Explicit per-run skips retain their existing behavior.
If the run also has a Test or CI approval override, `passed-with-override` takes precedence and the automatic skip causes remain visible.
Legacy rows without a recorded skip cause keep their prior classification; their logs remain inspectable.
For a completed run with an external CI owner, `pending-external-ci` takes precedence over override and skip outcomes while CI claims await the handoff owner. The Test override reason remains visible. If those claims cannot be read or validated, `external-ci-handoff-unavailable` reports the error instead; neither outcome certifies CI or authorizes merge. See [`axi ci-handoff`](#no-mistakes-axi-ci-handoff).

When the pipeline applied fixes, successful outcomes include a `fixes` table and a `help` instruction to acknowledge the misses and list those fixes for the user's review.

### Strict launch receipts

Supply `--launch-nonce` and `--validation-generation` together to bind a launch to an exact request instead of reattaching by branch and head alone.
Both identifiers must be 1–128 ASCII characters from `A-Z`, `a-z`, `0-9`, `.`, `_`, `~`, and `-`; they are non-secret correlation values and must not contain credentials.
This mode requires the same exact intent bytes on retries, whether supplied as a string, file, or stdin.

```sh
no-mistakes axi run --intent "the user's goal" \
  --launch-nonce request-42 --validation-generation validation-3 \
  --base-branch epic/foo
```

After durable creation or claim, AXI writes a TOON `launch_receipt` object to stdout **before** driving the run.
The receipt remains available to a caller that captures stdout even if driving later stops at a gate or fails.
It contains `run_id`, `disposition` (`created` or `reused`), `launch_nonce`, `validation_generation`, `branch`, `head_sha`, `submitted_head_sha`, and `intent_digest`.
Both head fields contain the full, immutable submitted commit SHA; `intent_digest` is the lowercase SHA-256 digest of the exact persisted intent bytes, including whitespace.
Raw intent is not included in receipts, and generic run/status output does not expose the nonce binding or intent digest.

The nonce is scoped to the repository and branch.
The first successful receipt claim returns `created`; subsequent matching claims return `reused` for that same run, including after the gate or pipeline head advances.
A conflicting submitted head, validation generation, intent, [pinned Pi profile](/no-mistakes/reference/global-config/#per-run-pi-profiles), or explicit [shipping mode/draft pin](#strict-shipping-and-review-receipts) is refused.
A different nonce creates a distinct run rather than reattaching to a same-head run; both post-receive creation and the up-to-date-push fallback follow this contract.
The up-to-date-push fallback preserves the latest same-head run's PR URL unless its recorded PR state is closed or merged, including when an explicit `--base-branch` retargets that PR.
An explicit `--base-branch` is persisted on creation and must match the stored per-run base on replay; omitting it on replay preserves the stored base.
A replay that adds `--no-publish-intent` against a run bound to publish the section is refused for the same reason; the stored omit decision folds in the global [`intent.publish_intent`](/no-mistakes/reference/global-config/#intent) default at creation, so a replay without the flag still matches a run whose row omits publication.
An external CI owner must match the stored owner exactly on strict-launch replay; omitting it does not match a run created with that owner.
Explicit `--model`/`--effort` must match a stored pin the same way; omitting both preserves it.
A conflicting claim does not consume the first `created` disposition.
Without the two proof flags, ordinary reattachment is unchanged, and historical runs without a nonce are not adopted into a proof binding.

## no-mistakes axi ci-handoff

Read a completed run's explicit external CI proof handoff as JSON:

```sh
no-mistakes axi ci-handoff --run <run-id>
```

This succeeds only for a completed run with `controller-ship-pr` ownership, skipped Push, PR, and CI steps, and 1–128 current pending Review CI claims from its approved Review. The JSON includes `outcome: pending-external-ci`, `run_id`, `external_ci_owner`, the run's source repository and branch, and each claim's provider check identity and PR comparison receipt. A missing, stale, or unreadable claim returns an error and a nonzero exit. The caller verifies the source identity, PR comparison, and live checks before merge. The handoff may advance from the Review-approved head only through helper-created correction commits or first-parent descendants recorded between the starting and ending heads of this run’s successful correction-agent invocation in its worktree. User or external commits outside that invocation, rewrites, and non-descendants block the handoff; restart Review at that head. The handoff does not assert that CI passed.

## no-mistakes axi review-receipt

Read the exact comparison recorded for a completed run as JSON:

```sh
no-mistakes axi review-receipt --run <run-id>
```

The command requires a verified terminal head and a current comparison receipt. It returns the run and source identity, any recorded PR URL and forge head, the local head, target branch and commit, merge base, raw diff digest, and receipt generation. It fails when the receipt is missing, points at a different head, or lacks the source repository or branch. The caller must reread the live PR and target immediately before publishing or merging; this stored receipt is evidence of what the run checked, not a claim that the forge still has those coordinates.

## no-mistakes axi respond

Answer the current approval gate and continue until the next gate, CI-ready decision point, or final outcome.

```sh
no-mistakes axi respond --action approve
no-mistakes axi respond --action fix --findings F1,F2 --instructions "optional guidance"
no-mistakes axi respond --action fix --findings F1,F2 --ignore F3
no-mistakes axi respond --action fix --add-finding '{"description":"...","action":"auto-fix"}'
no-mistakes axi respond --action skip
no-mistakes axi respond --run <id> --action approve
```

| Flag             | Type     | Default       | Description                                                          |
| ---------------- | -------- | ------------- | -------------------------------------------------------------------- |
| `--action`       | `string` | (none)        | `approve`, `fix`, or `skip`; required. A reviewer's open question is answered with [`axi answer`](#no-mistakes-axi-answer), not here |
| `--run`          | `string` | current-branch active run | Answer the gate of this run ID from any directory, without resolving the current branch or worktree |
| `--step`         | `string` | awaiting step | Step to respond to                                                   |
| `--findings`     | `string` | (none)        | Comma-separated finding IDs to fix with `--action fix`               |
| `--ignore`       | `string` | (none)        | Comma-separated finding IDs to decline with `--action fix`; see the accounting rules below |
| `--instructions` | `string` | (none)        | Guidance applied to selected findings with `--action fix`            |
| `--reason`       | `string` | (none)        | Operator's explanation for Test acknowledgment or exception approval |
| `--accept-gap`   | `string` | (none)        | Comma-separated Test finding IDs to accept as a strict-shipping gap, with `--action approve`, `--reason`, and `--covered-by` |
| `--covered-by`   | `string` | (none)        | Test names or paths covering the accepted gap (comma- or semicolon-separated or free text), recorded verbatim; requires `--accept-gap` |
| `--add-finding`  | `string` | (none)        | JSON finding object to add and fix                                   |
| `-y`, `--yes`    | `bool`   | `false`       | Auto-resolve subsequent eligible gates until a decision point or outcome |
| `--wait`         | `duration` | `8m`        | Maximum time for pre-drive reads and post-response driving before the caller must reattach |

Each non-abort response is bound to the [gate identity](#no-mistakes-axi-status) in the fresh run-status snapshot read by this command, including with `--run`. If that gate is answered or replaced before admission, the command exits with code 2 and a structured `error` containing `code: gate_superseded` and `current_gate` with the current identity and status. Read `axi status --run <id>` before deciding how to answer the new gate. The command never retries a stale response against a replacement gate. The TUI binds responses to the gate it displays. Aborting a run does not require a gate identity.

Declines are explicit. With `--action fix`, every finding the gate shows must appear in `--findings` or `--ignore`; a response that leaves one out is refused with the unaccounted IDs named and the gate stays parked, so a partial selection can never silently decline the findings it omitted. An ID in both lists, or an ID the gate does not show, is refused the same way. A finding a previous response for the same step already decided may be omitted to keep that decision, including when recovery parks the same round again. Naming a finding previously chosen to fix in `--ignore` is refused too, because reverting an applied fix is out of scope for a gate response. The validation fails closed on the state it reads: if the gate's findings or this step's earlier decisions cannot be read, the response is refused, the gate stays parked, and the refusal names what could not be read. See [Finding decision history](/no-mistakes/reference/pipeline-steps/#finding-decision-history) for how these decisions are stored and carried into later rounds.

Finding IDs are trimmed before validation. Missing or repeated gate finding IDs receive fresh IDs before the gate is shown, so each ID selects only one finding. An added finding with no ID, or an ID that collides with any gate finding, receives a fresh ID. Ignored findings and selections already recorded for a recovered round also reserve their IDs. See [Finding decision history](/no-mistakes/reference/pipeline-steps/#finding-decision-history) for how a fix response restores acknowledged work after recovery.

Every successful fix response echoes what it recorded in a `recorded:` object: `fixed` (the findings selected, including `--add-finding` items under their normalized IDs), `ignored` (the findings explicitly declined), and `kept` (the gate findings omitted that a previous response for this step had already decided). A finding the restored decision dispatched is reported under `fixed`, not also under `kept`. Approve, skip, and abort emit no disposition echo. They are not decision-free: a nonempty gate they resolve is recorded as a decline round, which is what later rounds read as `user_chose_to_ignore`. `--yes` resolution is unchanged: it selects every current finding.

For an explicitly authorized Test exception in an ordinary run, use `no-mistakes axi respond --step test --action approve --reason "the operator's explanation"`.
The reason is optional: approval without one remains effective, and a qualifying exception is reported with no operator reason supplied.
The step retains its findings and exit code, and the reason is durable local evidence in `step_results.approval_reason`.
Revalidation, a new fix round, or skipping the step clears that current-step approval so a later result cannot inherit it.
This is separate from the configured-command waiver and trusted repository opt-in used by [PR enforcement](/no-mistakes/reference/pipeline-steps/#pipeline-step-attestation); neither that policy nor approval authority changes.
`--instructions` remains fix guidance, not an approval-reason input.

A strict-shipping Test parked on an `inconclusive` verdict, where the unreachable scenarios are covered only by mocked tests, refuses plain approval and skip. The owner can accept that specific gap instead:

```bash
no-mistakes axi respond --step test --action approve \
  --accept-gap test-verdict \
  --reason "owner approved via first mate, 2026-10-14" \
  --covered-by "TestBrokerTimeoutMocked; internal/broker/timeout_test.go"
```

`--accept-gap` requires `--action approve`, a single-line `--reason` of at least 10 characters after trimming surrounding whitespace (up to 500 characters including that whitespace) that names who approved the gap and where, and a non-empty single-line `--covered-by` of at most 500 characters. Neither field may contain a Unicode control character (including C1 such as U+0085) or a line or paragraph separator (U+2028, U+2029); limits count Unicode characters, not bytes. Both strings are stored verbatim. The CLI probes the daemon first and refuses with ``the daemon predates accepted gaps; restart it with `no-mistakes daemon restart` `` when it cannot honor `--accept-gap`, because an older daemon would silently drop the fields. [Accepted Test gaps](/no-mistakes/reference/pipeline-steps/#accepted-test-gaps) owns eligibility, durable recording, and publication in receipts and the PR.

Plain `--step test --action approve` acknowledges a genuine strict `no-surface` Test; [strict shipping](#strict-shipping-and-review-receipts) owns its admission requirements.

Without `--run`, the response goes to the active run on the current branch, and `no active run to respond to` is the error when it has none. With `--run <id>`, the response goes to exactly that run, from any directory and without resolving a repository, branch, or worktree; every other flag behaves as it does in context. The call is refused with a nonzero exit when the ID names no run (`no run with id <id>`) or the run is not parked at a gate (`run <id> is not parked at a gate`), and it never answers a different run. [`axi status --run`](#no-mistakes-axi-status) stays inspection-only for bare commands and names `axi respond --run <id>` as the response command. An empty or whitespace-only `--run` value is refused with a nonzero exit and never selects the current branch's run. Every follow-up command a `--run` call prints carries `--run <id>`; a command with no `--run` form (`axi answer`, `axi run`, `axi sync`, `rerun`) is named as one that must run in the clone of that run.

After the explicit response, `--yes` uses the same [auto-resolution behavior and exceptions as `axi run --yes`](#no-mistakes-axi-run).
Each `axi respond` blocks until the next gate, CI-ready decision point, or final outcome, subject to the same default `--wait 8m` boundary as `axi run`. That boundary also covers its initial active-run and run-state reads plus event-subscription acknowledgement, so a caller can interrupt establishment as well as the later event wait.
An accepted response may wait for capacity under the [step-status contract](/no-mistakes/reference/pipeline-steps/#step-statuses). If `--wait` elapses during that wait, reattach with `axi run`; the response was already accepted.
If it returns another `gate:`, answer that gate; do not idle-wait for the run to move forward by itself.
When the daemon is already running, `axi respond` can continue an active run even if the global config file has become invalid, because it is not starting a fresh run.
The same successful-output reporting instructions apply to `axi respond` results.

## no-mistakes axi answer

Answer one question the run's reviewer asked while it was reviewing. See [The Review Conversation](/no-mistakes/concepts/review-conversation/) for the protocol and state machine.

Requires trusted [`review.conversation: true`](/no-mistakes/reference/repo-config/#reviewconversation), which is off by default: without it the reviewer was never told to ask, so this command refuses and names the setting. A question the reviewer already asked stays answerable if the setting is turned off mid-run - see [The Review Conversation](/no-mistakes/concepts/review-conversation/).

```sh
no-mistakes axi answer --question q1 --answer "Keep it behind a flag" --by captain
```

| Flag         | Type     | Default            | Description                                                              |
| ------------ | -------- | ------------------ | ------------------------------------------------------------------------ |
| `--question` | `string` | (none)             | Question ID, as carried by the review gate's `question-<id>` findings, or named by the gate's omission notice when more questions are open than the gate renders as rows; required |
| `--answer`   | `string` | (none)             | The answer, ideally one of the question's stated options; required       |
| `--by`       | `string` | (none)             | Who answered; recorded on the PR and in the branch's settled questions   |
| `--wait`     | `duration` | `8m`             | Maximum time the answer that closes the last open question blocks following the run, as for `axi respond`; any other answer returns at once |

The run is always the current branch's active run, and there is deliberately no `--run`: `axi`'s run resolution is branch-scoped, and this command mutates, so a second selection path could land an answer meant for one branch's reviewer on another's. Run it from a clone of the repository whose run it answers - the repository is resolved from the working directory, so a directory outside any initialized repository reports `repo not initialized` rather than answering.

This is not a gate response, and `axi respond` does not accept an answer. The answer is appended to the run's review conversation immediately, so a reviewer that is still working reads it at its next checkpoint and can redirect the rest of its pass. The output reports `open_questions` and `reviewer_resumed`; see [answer delivery](/no-mistakes/concepts/review-conversation/#notification-is-a-push-not-a-poll) for when the reviewer resumes and how a superseded gate is handled.

The answer that closes the last open question follows the run like `axi respond` after its response: it blocks until the next `gate:`, `checks-passed`, or final `outcome:`, bounded by `--wait`, and returns that document led by the answer's own fields. A review gate that still carries questions right after the answer is given one [`gate_reconcile_timeout`](/no-mistakes/reference/global-config/) to leave, because a park registered just after the answer landed briefly still lists the question it closed until the daemon releases it. A gate still there after the grace is returned. If the answer was recorded before gate registration, a [recovered gate](/no-mistakes/concepts/daemon/#crash-recovery) waiting for a run slot may outlast that grace and return the question you just answered; once admitted, it re-checks immediately rather than waiting a timer interval. A `--wait` at or below the grace elapses before the grace does, so even a genuine new question is reported as an elapsed wait rather than a `gate:`. If it returns another `gate:`, answer that gate. An answer that leaves questions open, or closes none, returns at once. An elapsed wait, and a park that came back already answered, both reattach with `axi run`, never by answering again.

Every answered question is recorded per branch, so a later cold reviewer receives it as settled and does not re-raise it, and the question and answer appear in the PR body's review conversation.

Answering requires an active run, because only its executor can resume the reviewer.

## no-mistakes axi status

When `--run` is omitted, show this branch's run: its active run, else its most recent one.
Resolution is scoped to the current branch and never falls back to another branch's run, because one clone commonly has several worktrees on different branches.
On a successful status response, when the current branch has no run of its own - including a detached `HEAD`, which owns no branch and so reports `current_branch: unknown` - the output carries no run object at all.
It reports `current_branch`, `runs_on_current_branch: 0` where a branch is known, and the recent-runs listing, so an unrelated run can never be read as this worktree's.
If the implicit current-branch lookup itself fails, status returns that error instead of presenting the failure as a detached or no-run result.
Detached-`HEAD` help offers deliberate `--run <id>` inspection or checking out a branch; it does not offer `axi run`, which requires a branch.
With `--run <id>`, inspect exactly that run regardless of branch; when its branch differs from a known current branch, it is rendered under `other_branch_run:` instead of `run:`, alongside a top-level `current_branch`, so a parser keyed on `run:` never picks up a run proven to be on another branch.
An explicit `--run <id>` rendered under `run:` while the current branch is unknown (detached `HEAD` or a branch-lookup failure) encodes no branch relationship.

```sh
no-mistakes axi status
no-mistakes axi status --run <id>
```

| Flag    | Type     | Default            | Description               |
| ------- | -------- | ------------------ | ------------------------- |
| `--run` | `string` | current-branch run | Inspect a specific run ID |

A parked `gate:` includes `run_id`, `step_result_id`, `round_id`, and `nonce`, alongside its step and status. The nonce changes on every park, even when the same round parks again. IPC run-status payloads expose the same identity as each step's `gate` object. Every non-abort IPC response must supply it as `expected_gate`; missing or mismatched identities are refused with `gate_superseded` and the current gate identity. Abort is exempt. Accepted pending responses always store their full identity and are replayed only against that same park after restart. Recovery refuses a park without a saved nonce and matching round ID, even when no response is pending; it also refuses a pending response with a missing or mismatched identity.

The resolved `run:` or `other_branch_run:` object uses the [shared AXI scheduling and gate output](#no-mistakes-axi), including the distinction between an unanswered gate and an accepted response waiting for a run slot.
When a run has an exact PR comparison receipt, `run.review_generation` identifies its current receipt generation. Compare this field with `axi review-receipt`'s `generation` as well as the run head and live target before accepting a terminal review result.
A pinned run also includes `pi_profile` with `model` and `effort`; see [per-run Pi profiles](/no-mistakes/reference/global-config/#per-run-pi-profiles).
Status offers branch-scoped `axi respond` commands only for the current branch's implicitly resolved run. An explicitly selected gate stays inspection-only even when its branch matches, because a newer active run on that branch could receive the bare response command instead; the gate remains visible, its log commands retain `--run <id>`, and it names `axi respond --run <id>` as the response command.
When a repository has no configured lint command and Document performs the combined Document/Lint housekeeping invocation, the run object includes `shared_work` evidence naming its `document+lint housekeeping` scope and the duration attributed to Document; Lint's own duration remains the cached-result handoff time.
When the resolved run has a `running` or `fixing` step, the run object includes `active_steps`.
Each row reports the whole step's elapsed time as `active_for`, the displayed execution or fix round's elapsed time as `round_active_for`, the latest meaningful log or native-agent lifecycle activity, the native agent PID if one is currently running, and the current round such as `round 1`, `auto-fix 1/3`, or `fix 2`.
`round_active_for` resets when a fix round starts; older active runs created before this timing was recorded show it as empty.
If no activity arrives for longer than `step_quiet_warning`, `last_activity` is prefixed with `quiet`; this is only a liveness signal and does not cancel the step.
For older active runs with no recorded activity timestamp, AXI falls back to the step log file modification time.
Finding descriptions are always rendered in full, so an `ask-user` finding can be relayed verbatim. Gate summaries are bounded in this default status view because a command gate's summary carries its command output; a truncated summary discloses its original length, and the gate help points to `no-mistakes axi logs --step <step> --full` for an implicitly resolved run or `no-mistakes axi logs --run <id> --step <step> --full` for an explicitly selected run.
Relevant current-branch states also include a cached `branch_sync` object with full SHAs, the run's status, the persisted pipeline push binding, target kind and ref, relation, safety result, PR lifecycle, and a structured next action.
Cached home and status rendering performs no network read and labels the remote observation `pipeline_push`; only explicit sync check or apply reports `live` freshness.

## no-mistakes axi sync

Freshly check or apply the guarded synchronization offered by a `branch_sync.next_action`.

```sh
no-mistakes axi sync --check
no-mistakes axi sync
no-mistakes axi sync --recover
no-mistakes axi sync --recover --keep-local
no-mistakes axi sync --recover --authoritative-head <full-sha>
no-mistakes axi sync --bind-archive-ref refs/heads/archive/<name>
no-mistakes axi sync --adopt-published
```

| Flag                 | Type     | Default | Description                                                                  |
| -------------------- | -------- | ------- | ---------------------------------------------------------------------------- |
| `--check`            | `bool`   | `false` | Verify the live target and exact plan without changing `HEAD`                |
| `--recover`          | `bool`   | `false` | Return custody of a branch stranded by a terminal run with unpublished pipeline commits (a no-op when cancellation already released the branch), or perform `recover_remote_rewritten` |
| `--keep-local`       | `bool`   | `false` | With `--recover`: keep the current local head; never touches the worktree   |
| `--bind-archive-ref` | `string` | (none)  | Bind one existing `refs/heads/archive/*` commit as exact evidence for a keep-local recovery; never creates or moves a Git ref |
| `--adopt-published`  | `bool`   | `false` | Adopt a clean diverged local head into its stale gate lane only when the configured push target has that exact head |
| `--authoritative-head` | `string` | (none) | With `--recover`: require the registered worktree to already be at this exact full commit SHA; never moves the worktree |

The default command is an explicit non-interactive apply request and never prompts.
All modes return the complete `branch_sync` object as TOON.
Exit code `0` means an eligible check, applied synchronization or recovery, already-synchronized, a live-verified custody-returned no-op, a user-owned no-op, or an expected merged-and-removed no-op; blocked operational states return `1`.
If the bound remote branch is missing and the recorded PR state is open or unknown, sync checks the live PR through the configured forge profile. For GitHub, the recorded PR URL must identify the same host, owner, and repository as the current lookup target; comparisons ignore case. An invalid URL or a different repository keeps sync blocked without recording an observation, even when an unchanged fork still has a valid push binding. A merged PR reports `merged_remote_removed` / `already_retired`; a closed PR reports `closed`. An open PR remains `blocked_remote_missing`. If the PR URL or SCM host is unavailable, or the lookup fails, sync remains blocked and offers `no-mistakes axi sync --check` to retry. A missing branch alone never proves a merge. `--check` keeps the observation in memory without updating run metadata. An explicit sync without `--check`, including the TUI's `u` action, records the observed state for later inspections, even when retirement means no confirmation or Git update follows; it does not complete the interrupted run. This missing-branch check changes no Git refs or worktree files.
The ordinary worktree mutation is either a strict fast-forward of the invoking clean checked-out branch to the freshly verified pipeline-owned pushed SHA, or an equivalent-diverged advance.
When a clean local branch and the pipeline-pushed head are diverged but the local unique work is content-equivalent to work already represented in the live pipeline head, `sync` reports `safety: safe_equivalent_advance`, anchors the pre-sync head under `refs/no-mistakes/sync-anchor/<run>`, and moves to the pipeline head with reset semantics.
Genuine divergence still reports `safety: blocked_diverged` and changes nothing during ordinary synchronization. Its `next_action.command` is `git checkout --detach <pushed-sha>`, which needs no approval, moves no branch ref, and loses no work; the local branch stays where it was.
Under `--recover`, the possible worktree mutation is a strict fast-forward to the preserved pipeline head, or an adoption of a preserved head proven to carry every local change, both after relation-specific preservation checks. The bound-archive exception described below never changes the worktree at all.
A terminal run whose recorded head was never verified (for example a Review fix and a Test fix followed by a Document fetch failure) is verified by recovery itself only from fresh gate state: either the live gate branch is an equal-tree rewrite of the exact reviewed result, or the recorded head extends the live gate branch as the reviewed result plus the pipeline's own later commits, the gate's non-symbolic `refs/no-mistakes/recover/<run>` ref pins that recorded head exactly, and the clean local checkout already sits exactly at it. Recovery never moves a worktree onto an unverified head; a behind or dirty worktree, a rebased head, a missing review authority, or a moved gate branch stays `blocked_recover_unverified_head`.
When the local gate branch is exactly at a newer same-branch pushed binding and Git proves that an older terminal run's unpublished preserved head is its ancestor, branch synchronization selects the newer binding; missing gate evidence, non-ancestor heads, or different or ambiguous target provenance remain blocked.
Fork configurations verify the configured fork URL and exact feature ref rather than assuming `origin`.
Dirty, in-progress, ahead, genuinely diverged, detached, wrong-branch, offline, changed-target, rewritten, deleted, legacy, or retired states fail closed without destructive recovery.
Run `axi sync` only when structured output offers `next_action.code: sync`; process any blocked state instead of substituting reset, stash, merge, rebase, force, or branch replacement.

### Rewritten push target recovery

When a fresh check finds the configured push target no longer equals the persisted push binding and the bound head is not its ancestor (the branch was force-rewritten outside the pipeline), it reports `state: remote_rewritten` and `safety: blocked_remote_rewritten` and never adopts the rewrite on its own. Unless the PR is merged or closed, the next action is `continue_active_run` while the owning run is active, or `next_action.code: recover_remote_rewritten` with the exact command `no-mistakes axi sync --recover` once that run is terminal. A merged or closed PR gets no recovery action for a rewritten remote.

That recovery re-reads the live target, anchors the superseded pipeline head under `refs/no-mistakes/recover-rewritten/<run>/<push_generation>` in the worktree or, when only the gate has it, the local gate, confirms the live head did not change again, and then compare-and-swaps the persisted push binding (`pushed_head`, the run head, and `push_generation`) to the verified live head. It reports `recovered: true` with `recovery.source: remote_rewritten`, never changes the worktree, a branch, the gate branch, or the remote, and does not return custody. `--keep-local`, a merged or closed PR with a rewritten remote (no action is offered there), a dirty or changed invoking worktree, an unanchorable superseded head, a live head that moved again, or a binding, run, or configured push target that changed during recovery refuse without rebinding. For any terminal run with a push binding (published or custody-returned), `--recover` reports `recovered: true` only when this rebind commits for the run that still owns the branch, or when a fresh live check proves the binding already equals the live head. The latter remains an idempotent no-op for a custody-returned run even if its PR is merged or closed. An offline, deleted, advanced, changed, or otherwise unverified target refuses with `recovered: false`. A successful rebind exits `0` without a top-level `error`; any relation to the new binding, such as divergence, is reported in `branch_sync.note` and `next_action`. Afterwards, follow the ordinary `next_action` reported against the new binding.

### Published-rebase gate recovery

A custody-returned branch can later be rebased and force-with-lease pushed to its configured target. Its local head then diverges from the preserved gate lane, so an ordinary gate push correctly rejects it as non-fast-forward. Status reports `state: custody_returned`, `relation: diverged`, and `next_action.code: adopt_published` instead of directing another rejected `axi run`.

`axi sync --adopt-published` is the explicit recovery. It requires a clean exact checked-out branch, the same recovered lane at its recorded preserved head, and a live configured push target whose branch exactly equals local `HEAD`. It fetches that verified object into the local gate, preserves the old gate head under the run's recovery ref, then compare-and-swaps only the current lane. It never pushes to the configured target or changes the worktree. A missing, changed, or different target head, a changed gate lane, or changed local assumptions refuses without replacing the lane.

### Custody recovery

A run that goes terminal (cancelled, failed, or completed without a push stage) after moving the pipeline head leaves the branch `pipeline_owned`. Status offers `next_action.code: recover_custody` only when recovery can establish the same eligibility it will enforce: an equal or ahead local head proves the source locally and can create the local anchor when the gate is unavailable, but any existing gate recovery ref must still match the recorded head; importing a missing preserved head requires an exact run-specific gate anchor (or legacy commit evidence that can be anchored), a clean worktree, and either local ancestry or the content-preservation proof described below. The archive-backed keep-local exception has its own stricter proof below. An eligible state reports `safety: blocked_pipeline_owned_recoverable`, the run's terminal `pipeline.status`, and the exact `submitted_head`/`current_head`/`relation` ownership facts.
A run whose terminalization verifies that the managed worktree head never changed from the submitted head releases the branch instead: the terminal outcome, including cancellation, ends ownership; status reports `state: user_owned` with the same exact ownership facts and no `next_action`; the branch and head are immediately usable for any separately authorized delivery; and nothing blocks a direct push or PR.
A run cancelled or superseded while queued, or failed or cancelled during setup before creating its worktree, records the same release: it created no pipeline commits or push binding, so branch synchronization requires no custody recovery.
Without positive evidence that the submitted head stayed unchanged, custody is not guessed away. Conflicting evidence, import cases with a dirty worktree, and genuinely divergent history retain manual-reconciliation guidance instead of being labeled as a missing preserved head. When a verified recorded head is absent from both the invoking worktree and an accessible local gate, with compatible recovery refs, status reports `safety: blocked_recover_preserved_head_missing` with `next_action.code: recover_custody` and `no-mistakes axi sync --recover --keep-local`: that flag is the operator's explicit choice to keep the current local head and discard the missing preserved commits. An unavailable gate remains blocked because absence cannot be established there. The archive-backed keep-local exception has its own stricter proof below.
While a run is still active, it reports `state: pipeline_owned`, the exact submitted/current heads and their relation, and `next_action.code: continue_active_run` with `no-mistakes axi status`, even when its head has not moved yet.
`--recover` verifies the run is terminal, anchors the preserved head under `refs/no-mistakes/recover/<run>` in the invoking repository, and stamps custody returned so a fresh run can start.
For equal or ahead worktrees where the preserved head is already locally reachable, recovery writes that anchor locally without requiring gate access. If the gate is available, an existing symbolic, non-commit, or mismatched recovery ref is conflicting evidence and recovery refuses without overwriting it.
For behind or diverged worktrees, recovery verifies the preserved head at the run-specific recovery ref in the local gate and fetches it into the anchor before moving or refusing. Legacy recorded heads that remain available as unreferenced gate objects are anchored before recovery continues.
A clean behind worktree fast-forwards.
A diverged worktree is adopted only when the preserved head provably carries every local change, proven by an executable three-way merge whose result is exactly the preserved head's tree.
This covers a pipeline rebase onto a newer base without requiring the gate branch to advance to the preserved head.
Terminalization pins an unpublished pipeline head under a run-specific recovery ref before recording it as verified, so recovery does not require the gate branch itself to have advanced. If the verification stamp is missing and the recorded head is ahead of the live gate branch head, recovery accepts it only when the run's own non-symbolic recovery ref names exactly that commit. The gate branch must directly name a commit too; recovery locks both refs in a Git transaction, checks their direct identities and exact commit targets under those locks, and keeps the locks through recording verification. An equal-tree recovery that creates an anchor commits it first, then locks and revalidates both refs before recording verification. If either changed, it refuses without a verification stamp. Status applies the same proof before offering recovery; without it, this case requires manual reconciliation. `no-mistakes rerun` selects that anchored head too, subject to its caller-head checks below; it also refuses a symbolic gate branch, and without the anchor proof it retains the gate-head selection. Recovery can also verify a live gate head equal to or descended from the recorded head, or an equal-tree rewrite of the exact reviewed result that preserves local work. These cases still require the ordinary recovery safety checks.
That adoption anchors the pre-recovery local head under `refs/no-mistakes/recover-local/<run>`, then moves the branch with Git operations that refuse on their own rather than after a preceding check: an atomic compare-and-swap on the branch ref, and a working-tree update that aborts instead of overwriting a modified or untracked file.
The proof is deliberately narrow and never uses patch identity, which discards hunk locations and whitespace and so cannot tell a genuine replay from a same-shaped edit elsewhere.
Anything it cannot decide - unlanded local commits, or a rebase whose fix rounds also rewrote your own lines - still refuses with the anchor named, because only escalation can tell a deliberate pipeline fix apart from a dropped change.
A dirty worktree refuses with explicit choices.
When you explicitly keep a behind or diverged local head instead of taking the preserved head, `--keep-local` returns custody at the current head without touching the worktree and atomically points the gate branch at it. `--keep-local` is also the recovery when an accessible gate confirms that the verified recorded head is missing and the recovery refs are compatible: the operator is keeping the current local head and discarding those unpublished commits, so that missing object does not block the path. One `--keep-local` call releases the full stranded stack: genuinely missing heads are explicitly discarded, while still-available heads are anchored under their run-specific recovery refs first. Unverified or conflicting evidence on any run blocks the release before any custody stamp. If the gate branch moved independently, recovery first preserves that head under `refs/no-mistakes/recover-gate/<run>`; a conflicting pre-existing anchor makes recovery refuse, and a concurrent gate push wins the compare-and-swap and also makes recovery refuse.

A narrower archive-backed path handles a divergent later validation head that must be preserved but must not become the working result. First create the archive ref through a trusted preservation workflow, then explicitly bind that already-existing ref with `--bind-archive-ref`. Binding writes only an append-only recovery-archive record; it never creates, moves, fetches, or deletes a Git ref. The record snapshots the repository ID, lookup and evidence run IDs, branch, exact required working head, exact preserved later head, archive ref, and creation time.

Status uses the same `recoverySourceAvailable` classification as recovery and accepts this path only when exactly one record matches the selected terminal run, the clean checked-out branch remains at an exact submitted, reviewed, or successfully pushed required head, the later recorded head genuinely diverges, the raw archive ref is a non-symbolic commit at that exact later head, and the gate's run-specific recovery ref independently still pins it. The gate branch itself must be at either the required head or the preserved head. It revalidates the repository, run, branch, both full SHAs, archive ref, gate branch, and every existing recovery anchor before each action. Missing, moved, replaced, symbolic, malformed, cross-repository, wrong-run, wrong-branch, stale, hash-mismatched, or multiple records fail closed with `next_action.code: inspect_and_reconcile_manually`; similarly named branches, reflogs, loose objects, remote-tracking refs, and tags are never scanned or inferred as proof.

A verified archive plan reports its evidence in `branch_sync.recovery`, keeps `next_action.code: recover_custody`, and sets the command to exactly `no-mistakes axi sync --recover --keep-local`. That action leaves the worktree and local branch at `recovery.required_head`, leaves the divergent archive at `recovery.preserved_head`, and never merges, replays, fast-forwards, resets to, or otherwise selects the archived history. Running plain `--recover` against this plan refuses without adding an anchor or moving a ref.

For the alternative validation path and its refusal conditions, see [`no-mistakes rerun`](#no-mistakes-rerun); it is not offered for the archive-backed keep-local plan.
A recovered never-pushed run reports `state: custody_returned`; a recovered pushed run reports its ordinary classification against the last push binding, typically `local_ahead`.
On a `user_owned` branch, `--recover` is an idempotent no-op success: nothing pipeline-created exists to recover, and no file, ref, or database row changes.

## no-mistakes axi logs

Show one pipeline step's recorded findings and log output.

```sh
no-mistakes axi logs --step review
no-mistakes axi logs --step review --full
no-mistakes axi logs --step review --run <id>
no-mistakes axi logs --step gate.test.mutation-budget
```

| Flag     | Type     | Default            | Description                                                                          |
| -------- | -------- | ------------------ | ------------------------------------------------------------------------------------ |
| `--step` | `string` | (none)             | Step name; required                                                                  |
| `--run`  | `string` | current-branch run | Run ID to inspect                                                                    |
| `--full` | `bool`   | `false`            | Show the complete summary and the entire log instead of the bounded summary and tail |

When `--run` is omitted, the run is resolved the same way as [`axi status`](#no-mistakes-axi-status): this branch's run, never another branch's.
With `--run <id>`, logs are read from exactly that run regardless of branch.
An unknown explicit run ID exits nonzero with `error: run "<id>" not found` instead of reporting that the current branch has no run.
When the step recorded findings, the output leads with its `summary` and a `findings` table whose descriptions are always complete, including after the step's gate was resolved.
If the recorded findings cannot be parsed, a `findings_error` field reports the parse error in their place and the step log still renders.
Without `--full`, the summary is bounded like the gate's and long logs show the last 40 lines; when either is cut, a help hint names the `--full` command, retaining the run ID when `--run <id>` selected the log.
Step logs include native subprocess agent lifecycle lines such as `codex started pid=4242`, `codex exited pid=4242 status=success`, and transient retry messages when the selected agent supports lifecycle events.
They also include fix-loop markers such as `auto-fix round 1/3 starting after round 1` and `user-fix round starting after round 2`.
`--step` accepts the nine core step names and valid repository gate names such as `gate.test.mutation-budget`. Use the exact gate name shown by `axi status`.

## no-mistakes axi abort

Cancel the active run for the current branch.
Active runs on other branches are left alone.

```sh
no-mistakes axi abort
```

If there is no active run, this succeeds as a no-op.

Pass `--run <id>` to cancel a specific run by its id instead of resolving the current branch:

```sh
no-mistakes axi abort --run <id>
```

`--run` does not need a repo, branch, or worktree, so it works from anywhere.
Use it to reap an orphaned CI monitor whose worktree was torn down before the PR merged - the run id is shown in `axi run` output and in the `axi` home view.
A `--run` id that is not currently active is resolved against the exact run's durable record rather than trusted blindly: a known already-terminal run returns an idempotent success carrying its terminal `run_status` with no fabricated new cancellation, a positively proven unknown id keeps the documented successful no-op with no fabricated state, and a run that is recorded as still nonterminal or cannot be read returns the nonzero terminal-unconfirmed contract.
When the daemon is not running, nothing can be cancelled and abort never starts one: the durable record alone decides the same three outcomes, and a recorded nonterminal run reports that cancellation could not be requested.
When the daemon is already running, `axi abort` can cancel an active run even if the global config file has become invalid, because it is not starting a fresh run.
Both abort surfaces report a completed cancellation only after the exact run positively confirms a terminal state within the bounded wait; success then includes the terminal `run_status`, and branch-scoped abort renders the refreshed `branch_sync` object and its exact next action, if any.
When terminal quiescence cannot be confirmed - the bounded wait expires, the wait is cancelled, or a status read fails - abort exits nonzero, states explicitly that cancellation was requested but terminal quiescence is unconfirmed, includes the last structured run state when one is available, and never claims `aborted: true` or presents user-owned or recoverable ownership guidance as authoritative; re-run the abort or watch `axi status --run <id>` until a terminal status is confirmed.
Pipeline-created commits remain preserved in the gate and a recoverable cancellation points directly to `no-mistakes axi sync --recover`; when the submitted head never moved, cancellation instead reports `state: user_owned` with no sync action.
While a run is active, do not use `axi abort` or `no-mistakes rerun` to go fix a finding yourself.
That cancels the pipeline's in-flight work and forces a full re-validation; use `axi respond --action fix` at the gate so the pipeline applies and re-checks the fix.

## no-mistakes eject

Remove the gate from the current repository.

```sh
no-mistakes eject
```

Removes the `no-mistakes` remote, deletes the bare repo directory, cleans up worktrees, and deletes the database record (cascades to runs and steps).
It does not remove any legacy repo-local agent skill files left by older versions; current `init` installs the skill at user level instead.

## no-mistakes attach

Attach to the active pipeline run.

```sh
no-mistakes attach [--run <id>]
```

| Flag    | Type     | Default | Description                                           |
| ------- | -------- | ------- | ----------------------------------------------------- |
| `--run` | `string` | (none)  | Attach to a specific run ID instead of the active run |

Opens the TUI for the active run anywhere in the current repo. If `--run` is specified, attaches to that specific run regardless of branch. Unlike bare `no-mistakes`, this does not stay branch-scoped before falling back.

## no-mistakes rerun

Rerun the pipeline for the current branch.

```sh
no-mistakes rerun
no-mistakes rerun --intent "the revised user goal"
no-mistakes rerun --model openai-codex/gpt-5.4 --effort high
no-mistakes rerun --no-publish-intent
no-mistakes rerun --closes 95
```

`--model` and `--effort` opt this new run into a [pinned Pi profile](/no-mistakes/reference/global-config/#per-run-pi-profiles), with the same precedence and validation as `axi run`. Omitting both retains current global-config behavior; a prior run's model pin is not inherited.

Starts a new pipeline run from the current gate branch, except when the latest
terminal run has an unpublished head that is verified or has the exact
recovery-ref proof described under [Custody recovery](#custody-recovery), and
custody has not been returned: rerun then uses that preserved terminal head even
if the gate branch is stale. Once that terminal head is selected, the command
refuses instead of falling back to the gate branch when the run-specific
recovery ref is conflicting, invalid, or the recorded head is unavailable.
Use `no-mistakes axi status` and reconcile custody first in that case.
An immutable-source run selects the exact submitted head from the selected run
only while that commit is still the live published source branch. Rerun does not
depend on or move a stale or absent local gate branch. It preserves the run's
shipping target branch, target SHA, and draft choice; a conflicting
`--base-branch` or a moved live target is refused before superseding a run.
If the gate lacks the commit, rerun fetches that exact published commit before
it cancels or creates a replacement run; the fetch does not move a local branch.
When invoked from a clean worktree, `rerun` also refuses if that worktree's HEAD
differs from the selected gate or preserved head, before starting or superseding
any run. The error names both full commit SHAs;
inspect `no-mistakes axi status` and follow its custody guidance, then use
`no-mistakes axi run` to submit local commits. The refusal leaves both branches
unchanged; rerun never replaces its selected head with the caller's head.
The same check applies to `axi run`'s rerun fallback after an up-to-date push.
Dirty worktrees and callers without clean-head evidence, including TUI reruns,
retain the existing selection behavior.
If the selected prior run has explicit intent, rerun inherits it exactly by default;
otherwise it performs fresh intent inference. `--intent` supplies a new canonical
explicit intent in either case. Inherited intent keeps distinct rerun provenance;
an override is recorded as newly supplied explicit intent, while fresh inference
records the transcript source. Omission of the generated Intent section is always
inherited from the selected prior run; `--no-publish-intent` and the global
[`intent.publish_intent`](/no-mistakes/reference/global-config/#intent)
default can only add omission, never remove it. If another run is active on that branch, rerun
cancels it before starting over. Treat rerun as a between-runs action after a
failed or cancelled outcome; use `axi run` for separate local fixes, and do not
use rerun to bypass a gate.

| Flag | Type | Default | Description |
| ---- | ---- | ------- | ----------- |
| `--intent` | `string` | (none) | Explicit intent overriding inherited intent or fresh inference |
| `--no-publish-intent` | `bool` | `false` | Keep the generated `## Intent` section out of the PR body for this rerun (adds to the inherited decision; tighten-only) |
| `--closes` | `string`, repeatable | (none) | Issue the PR fully resolves; adds to the [closing references](#closing-issues) inherited from the selected prior run |
| `--model` | `string` | (none) | Pi provider/model ID for an immutable [per-run profile](/no-mistakes/reference/global-config/#per-run-pi-profiles) |
| `--effort` | `string` | (none) | Pi reasoning effort for that profile; omitted fields inherit `agent_config.pi` |

## no-mistakes sync

Freshly verify and, with confirmation, safely move the invoking branch to an exact pipeline-owned push binding.

```sh
no-mistakes sync
no-mistakes sync --check
no-mistakes sync --yes
no-mistakes sync --recover
no-mistakes sync --recover --keep-local
no-mistakes sync --bind-archive-ref refs/heads/archive/<name>
no-mistakes sync --adopt-published
```

| Flag                 | Type     | Default | Description                                                     |
| -------------------- | -------- | ------- | --------------------------------------------------------------- |
| `--check`            | `bool`   | `false` | Verify and print the fresh plan without changing `HEAD`         |
| `-y`, `--yes`        | `bool`   | `false` | Apply an eligible guarded synchronization without an interactive prompt |
| `--recover`          | `bool`   | `false` | Return custody of a branch stranded by a terminal run with unpublished pipeline commits (a no-op when cancellation already released the branch), or perform `recover_remote_rewritten` |
| `--keep-local`       | `bool`   | `false` | With `--recover`: keep the current local head; never touches the worktree |
| `--bind-archive-ref` | `string` | (none)  | Bind one existing `refs/heads/archive/*` commit as exact keep-local recovery evidence without changing Git refs |
| `--adopt-published`  | `bool`   | `false` | Adopt a clean diverged local head into its stale gate lane only when the configured push target has that exact head |

Without `--yes`, apply prints the exact full-SHA plan and requires TTY confirmation when synchronization remains eligible; `--recover` and `--adopt-published` also prompt before their guarded changes. Rewritten-remote recovery confirms that it will anchor the superseded head and rebind the recorded push binding without moving the worktree. Archive binding is itself explicit, does not prompt, and cannot be combined with synchronization, recovery, or `--yes`.
When confirmation is required, non-TTY input refuses with a direct `--yes` hint.
The command uses the same service and safety contract as `no-mistakes axi sync`, including the guarded equivalent advance and custody recovery documented there; it never stashes, rebases, creates a merge commit, switches branches, deletes a branch, or updates an external remote.

## no-mistakes status

Show repo, daemon, active run, and relevant cached local-branch synchronization status.

```sh
no-mistakes status
```

Displays:

- Repo path, upstream URL, and fork URL when configured
- Gate path
- Daemon status (running/stopped, PID)
- Active run details: ID, branch, status, head SHA, start time

## no-mistakes runs

List recorded pipeline runs for the current repo.

```sh
no-mistakes runs [--limit <n>]
```

| Flag      | Type  | Default | Description                       |
| --------- | ----- | ------- | --------------------------------- |
| `--limit` | `int` | `10`    | Maximum number of runs to display |

Shows runs newest-first with branch, status (styled), short SHA, timestamp, and PR URL if set.

## no-mistakes eval

Inspect the locally collected review-case corpus before spending tokens, replay an explicit agent and model with isolated no-mistakes state and a throwaway worktree, and report finding-level scores, token cost, wall time, and the recall-versus-cost frontier. Eligible cases are collected automatically as runs finish; `eval capture <run-id>` collects one on demand; `eval miss ingest <run-id> --finding '<json>'` labels a confirmed post-PR miss (review passed green, later caught) as false-negative gold.

See [Evaluation toolkit](/no-mistakes/reference/eval/) for the local-only boundary, collection and retention, command flags, label policy, and reporting semantics.

## no-mistakes stats

Show historical usage stats across all repos.

```sh
no-mistakes stats
```

Displays total changes, rescued changes, rescue rate, reported and fixed mistakes, fixes by pipeline step, and the top repos by rescue activity.

Use `--agents` for local, per-purpose agent performance aggregates: duration and the subprocess-vs-model time split, session mode, errors, the token totals (input, output, cache-read, cache-creation, fresh input, reasoning), and the model round-trip and tool-category activity histogram, with a `METRICS` coverage count that tells a real zero apart from missing instrumentation.
Use `--run <id>` to inspect the individual agent invocations for one run - including each invocation's per-round token deltas next to the adapter counters, tool-category breakdown, workload size, finding count, and fallback reason - plus the total time parked at approval gates; it implies `--agents`.
Pinned runs also print the requested [Pi profile](/no-mistakes/reference/global-config/#per-run-pi-profiles) above that table; `MODEL` there is served evidence, not the pin.
The combined Document/Lint invocation is labeled `housekeeping (document+lint)` and attributed to `document+lint`, making its shared duration and tokens explicit without adding a second agent call.
See [local telemetry](/no-mistakes/reference/environment/#what-stays-local-and-what-leaves-the-machine) for token definitions and missing-value semantics.
A `--agents` token total is all-or-nothing: it reads `-` for the whole purpose unless every invocation in it reported that field, so one failed round that reported no usage leaves the group's total unknown instead of silently under-counted.

```sh
no-mistakes stats --agents
no-mistakes stats --run <id>
```

This detailed performance evidence stays local in `state.sqlite`; it is not sent to telemetry.
The field definitions and their local/remote split are owned by [the environment reference](/reference/environment/#what-stays-local-and-what-leaves-the-machine).

## no-mistakes doctor

Check system health and dependencies.

```sh
no-mistakes doctor
```

Checks:

- `git` binary
- `gh` CLI (reported as optional by `doctor`; required for guarded GitHub network runs, including PR and CI)
- `az` CLI (reported as optional by `doctor`; required for guarded Azure DevOps network runs, including PR and CI)
- Data directory (`~/.no-mistakes/`)
- SQLite database
- Daemon status
- Agent runners: native binaries `claude`, `codex`, `grok`, `acli`, `opencode`, `pi`, `omp`, `copilot`, and `agy` (Antigravity), plus the optional ACP bridge `acpx`
- ACP alias default binaries: `cursor-agent` plus `acpx` for `cursor`, and `devin` plus `acpx` for `devin`
- Effective global agent configuration, reported as `gate validation`; an unavailable configured runner is a failed check because the gate cannot validate without it
- Every configured [`forge_profiles`](/no-mistakes/reference/global-config/#forge_profiles) entry, reported as `forge <host>`: the profile resolves and validates, its provider CLI is installed, and that CLI is authenticated for the profile's host
- Every configured [`provider_plugins`](/no-mistakes/reference/global-config/#provider_plugins) entry, reported as `plugin <name>`: that its command executable resolves

Uses indicators: `✓` (available), `–` (not found, optional), `✗` (problem detected).

The standalone runner rows inspect default binary names; each ACP alias row (`cursor`, `devin`) reports whichever of its command binary and `acpx` are missing.
The [Global Config Reference](/no-mistakes/reference/global-config/) owns ACP gate-validation availability and probing semantics.
Each validation run performs the authoritative agent resolution again after applying any trusted repository-level override.

When the daemon is running, `doctor` also prints one `run slots` line with the `daemon.max_concurrent_runs` cap, slots in use, queued runs, and the number of live runs that released their slot while parked, for example `2/3 in use, 4 queued, 1 parked (slot released)`.

`doctor` checks `gh`, `az`, and `origin` (Cursor Origin) availability, and that each configured [provider plugin](/no-mistakes/reference/global-config/#provider_plugins) command resolves (it does not run the plugin's handshake). [Provider Integration](/no-mistakes/guides/provider-integration/) owns the separate setup checks for GitLab, Forgejo, Bitbucket Cloud, Gitea, and the Azure DevOps extension and PAT.

`tea` stays docs-only like `glab`, `forgejo-axi`, and Bitbucket's env vars, rather than an active `doctor` check like `gh`/`az`: Gitea is almost always self-hosted, so a bare "`tea` not found" row would be a near-universal, low-value warning for the vast majority of users who have no Gitea instance at all.

## no-mistakes update

Update the installed binary and reset the daemon.

```sh
no-mistakes update
no-mistakes update --beta
no-mistakes update -y
no-mistakes update --force
```

Downloads the latest release, verifies the SHA-256 checksum, atomically replaces the running binary, and resets the daemon when it is running or stale daemon artifacts exist so the new executable is picked up, preferring the managed service path and falling back to a detached daemon if service startup is unavailable or fails.
By default this installs the latest stable release.
Pass `--beta` to include prereleases and install the latest beta when one is newer than the current stable release.
Version discovery reads the configured fork's `channels.json` release asset to avoid the unauthenticated REST API rate limit. Stable, beta, and background checks use the same manifest. If the manifest is unavailable or invalid, update fails without falling back to upstream or the REST API. [Fork release prerequisites](/no-mistakes/guides/fork-updates/) explains publication and version ordering. Channel-manifest publication is a prerequisite; channel fallback is not implemented.
If the daemon is running from a different executable path, update still prompts before replacing it; pass `-y`/`--yes` to answer that prompt non-interactively.
If the daemon executable path cannot be determined, the update aborts before replacement.
If the daemon does not come back cleanly after a successful replacement, the command reports that failure.
When the running binary resolves into `/nix/store`, update prints that self-update is unavailable for Nix installs and exits without a version check; upgrade through Nix instead (see [Installation](/no-mistakes/start-here/installation/#nix)).
On macOS, removes the quarantine extended attribute.
[Daemon & Worktrees](/no-mistakes/concepts/daemon/#starting-and-stopping)
owns the active-run guard, the scope of `--force` and `--yes`, and recursive
validation-step containment.

Fork release binaries include the default self-hosted telemetry host and website ID. Disable telemetry with `NO_MISTAKES_TELEMETRY=0`, or override the host and website ID with `NO_MISTAKES_UMAMI_HOST` and `NO_MISTAKES_UMAMI_WEBSITE_ID`.

Background update checks run automatically on each CLI invocation (except `update` itself and version queries `--version` / `-v`, which stay side-effect-free). If a newer version is available, a notification is printed to stderr. Suppressed for dev builds, for Nix-store installs, or when `NO_MISTAKES_NO_UPDATE_CHECK=1` is set.

## no-mistakes daemon start

Start the daemon, installing or refreshing the managed service when possible.

```sh
no-mistakes daemon start
no-mistakes daemon start --force
```

Prefers the managed service path and falls back to a detached daemon if service install or startup is unavailable or fails. If the daemon is already running, the command refreshes a stale macOS `launchd` or Linux `systemd` service definition and restarts through the managed service; if the definition is unchanged, it reports that the daemon is already running. That restart passes the same active-run guard as `daemon restart`, so `--force` is needed to refresh a definition while pipeline runs are active. [Daemon & Worktrees](/no-mistakes/concepts/daemon/#starting-and-stopping) owns the active-run guard, the scope of `--force`, and the startup readiness, timeout, fallback cleanup, and singleton lifecycle details.

## no-mistakes daemon stop

Stop the running daemon process.

```sh
no-mistakes daemon stop
no-mistakes daemon stop --force
```

[Daemon & Worktrees](/no-mistakes/concepts/daemon/#starting-and-stopping)
owns the active-run guard, the scope of `--force`, and recursive
validation-step containment.

This does not remove the managed service. A later `no-mistakes`, `no-mistakes daemon start`, `init`, `attach`, `rerun`, or `update` can start the daemon again through the same service manager when available, or as a detached daemon otherwise.
On macOS, the retained LaunchAgent starts the daemon again at the next login; the stop output names its plist and points to `daemon uninstall`.

## no-mistakes daemon set max-concurrent-runs

Change how many pipeline runs execute at once, without restarting the daemon.

```sh
no-mistakes daemon set max-concurrent-runs 5
no-mistakes daemon set max-concurrent-runs 0   # unlimited
```

Writes `daemon.max_concurrent_runs` into the global config (only that value line changes; comments and other settings are preserved, and the block is created when absent), then asks the running daemon to apply it immediately and prints the resulting slot usage. A value that is not a whole number of 0 or more is rejected before anything is written. If `config.yaml` is a symlink, the target is updated in place with its own file mode and the link is left intact. With the daemon stopped, the value is only written and applies at the next start.

The global [`daemon` configuration](/no-mistakes/reference/global-config/#daemon) owns hot reload, admission order, and the effects of raising or lowering a cap. The [validation-step containment rule](/no-mistakes/concepts/daemon/#starting-and-stopping) also applies to `daemon set`, before any config write.

## no-mistakes daemon uninstall

Stop and remove the macOS LaunchAgent for the current `NM_HOME` instance.

```sh
no-mistakes daemon uninstall
no-mistakes daemon uninstall --force
```

Removes the LaunchAgent plist and names it in the output. If no LaunchAgent is installed, it succeeds with a message saying so. On other platforms it changes nothing, prints that no service removal is available there, and exits 0. It preserves application data and does not stop a detached daemon when no managed service exists. A later `daemon start` or a command that ensures the daemon is running can install the service again.

[Daemon & Worktrees](/no-mistakes/concepts/daemon/#starting-and-stopping) owns the active-run guard, the scope of `--force`, and recursive validation-step containment.

## no-mistakes daemon restart

Restart the daemon.

```sh
no-mistakes daemon restart
no-mistakes daemon restart --force
```

Stops the current daemon and starts it again. This works whether the daemon is currently running or not.
[Daemon & Worktrees](/no-mistakes/concepts/daemon/#starting-and-stopping)
owns the active-run guard, the scope of `--force`, and recursive
validation-step containment.

## no-mistakes daemon status

Check whether the daemon is running.

```sh
no-mistakes daemon status
```

Shows the PID if the daemon is running.

## Strict shipping and review receipts

The explicit `axi run --shipping-mode full` mode owns the complete Intent → Rebase → Review → Test → Document → Lint → Push → PR → CI pipeline. It persists its policy on the selected run and refuses launch if the running daemon does not advertise strict-shipping support. It refuses skips, passing command exceptions, and approval of unresolved actionable findings. `--yes` stops at those gates. A genuine `no-surface` Test may complete after an explicit operator acknowledgment, with or without `--reason`, once documentation correctness is confirmed. Every scenario must be named, untested, not live, and carry a reason; an empty scenario list is refused. Only the native no-surface decision finding may remain actionable. The durable record retains the original verdict, scenarios, and acknowledgment through recovery. This is normal completion, not a Test override. Live or pass/fail scenarios, malformed evidence, operator-authored lookalikes, command failures, recorded overrides, invocation-budget cuts, and unrelated actionable findings remain refused. An owner can accept a specific `inconclusive` Test gap with `axi respond --accept-gap`; see [Accepted Test gaps](/no-mistakes/reference/pipeline-steps/#accepted-test-gaps). Independent Review, hosted CI, App approval, and merge safeguards still apply. Repairs and changes after Review must pass a fresh independent Review before publication. A changed target comparison invalidates readiness. Periodic CI reads wait at least five minutes; initial reads, explicit receipt exports, and event-driven reads remain immediate.

Existing-PR target checks and readiness invalidation are owned by the [PR and CI step contracts](/no-mistakes/reference/pipeline-steps/#pr).

Use `--shipping-mode immutable-source --base-branch main` for an existing published protected source promotion. It pins the submitted source and current target. Rebase observes the comparison, and Push verifies the already published source; neither writes that source. Validation may inspect it, but source mutation or a repair request refuses the run. A moved source or target requires a new run. Terminal branch synchronization is inspection-only: `axi sync --check` reports `state: immutable_source` and never offers a mutating action. Direct sync, recovery, and adoption mutations refuse this mode.

With `--shipping-mode`, an optional `--draft` or `--draft=false` pins the GitHub draft selection for that run. Receipt export refuses a live draft state that differs from an explicit pin. Without the flag, existing repository/global draft configuration applies and the receipt records the live draft state. Reattach with ordinary `axi run` from the initiating branch; omit pin flags to retain the selected run's policy. An explicit conflicting mode or draft pin refuses. The caller's current head can be the submitted head or the current pipeline head while the daemon owns its run worktree.

```sh
no-mistakes axi run --shipping-mode full --draft --intent "Validate and ship this change"
no-mistakes axi review-receipt --run <run-id> --phase review
no-mistakes axi review-receipt --run <run-id> --phase ci-ready
```

`review-receipt` emits JSON on success, unlike ordinary TOON AXI output. Errors retain the ordinary structured AXI error format. An ordinary run may expose a completed observed JSON receipt, but that receipt cannot be signed and does not establish strict-shipping readiness; strict signing and readiness claims require the strict receipt phases below. It reads the recorded run without changing it. `--run` is required; `--phase` defaults to `review`. The `review` phase requires all configured pre-CI validation and publication steps completed, with an active CI monitor and an exact approved Review generation. It is available while CI awaits external approval. `ci-ready` also requires persisted readiness for that comparison and fresh, nonempty, successful applicable live checks (optional skipped hosted lanes retain native provider semantics). Every configured [custom gate](/no-mistakes/reference/repo-config/#gates) must be completed; export never silently omits one. Overrides, skips, dirty worktrees, unsupported providers, and moved source/target comparisons refuse either phase. Unresolved findings refuse either phase except under the documented Test acknowledgment and [accepted-gap](/no-mistakes/reference/pipeline-steps/#accepted-test-gaps) contracts. An empty check list or a no-CI declaration cannot establish `ci-ready`. The initial exact-forge proof provider is GitHub.

Both phases bracket live PR and Git observations and recompute the merge base and SHA-256 of the exact untrimmed bytes from `git diff --no-ext-diff --no-textconv --binary --full-index TARGET...HEAD`. The payload includes source repository/ref, submitted/local/forge heads, PR URL, target ref/SHA, merge base, diff digest, monotonic per-run review generation, intent digest, mode/draft, final non-actionable findings and their digest, and completed/running step evidence. The eight pre-CI core steps are completed; CI remains running until merge. The receipt grants no merge or cleanup authority.

Receipt export admits an acknowledged genuine `no-surface` Test by checking its retained durable evidence and authorization. The payload records Test as completed and keeps `test_override: null`; it does not export the Test verdict, scenarios, or acknowledgment reason.

`--signing-key-stdin` reads a bounded base64 32-byte Ed25519 seed outside authenticated validation-agent context. The command constructs and verifies its own payload; it never signs caller JSON. No key enters daemon IPC, agents, storage, logs, or errors. Provision and pipe the seed through the outer credential owner. Signed output is `{schema_version:1,algorithm:"Ed25519",key_id,payload,signature}`. `key_id` is SHA-256 of the raw public key; `payload` and `signature` use padded standard base64. The signature covers the UTF-8 bytes `no-mistakes-review-v1`, a NUL, then exact payload bytes. The decoded payload limit is 32 KiB and envelope limit is 49 KiB. Consumers must pin a trusted public key externally, authenticate bytes before parsing, and enforce current repository/PR/comparison identity and freshness.

Strict status adds `submitted_head_sha`, `shipping_mode`, `source_repo`, `source_branch`, `target_branch`, `target_sha`, `review_generation`, `ci_ready`, and `ci_ready_no_ci`. Enrollment supplies source repository/branch and the persisted target branch before Review. Target SHA and review generation remain null before capture. Status readiness is cached observability; use a fresh receipt before a guarded merge. A strict parked Review recovered after daemon restart cannot reconstruct generation authority from its head alone; operator approval cannot complete it.

For a parked `full` shipping Review, revalidate through the existing gate response: `no-mistakes axi respond --run <id> --action fix --findings <selected-ids> --ignore <explicitly-declined-ids> --instructions "Verify the existing fix against the pinned comparison; preserve correct code and prior commits."` Account for the live gate's IDs; previously decided findings may instead be omitted to preserve their decisions. This runs the normal fixer and a fresh independent Review, captures a new comparison generation, and retains the outstanding set until the [Review resolution contract](/no-mistakes/reference/pipeline-steps/#review) is satisfied. This path works for live and recovered gates; it grants no operator override. `immutable-source` still refuses repair responses: use its existing between-runs lifecycle only after the active run has ended, and never bypass its source or target movement checks.

Strict shipping uses the persisted per-run target throughout validation. With no `--base-branch`, that target is the enrolled default branch; ordinary runs retain their configured PR-base behavior.
