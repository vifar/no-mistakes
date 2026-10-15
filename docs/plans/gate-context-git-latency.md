# Gate-context Git latency mitigation

## Intent and evidence

Remove an unnecessary Git subprocess from ordinary-clone classification and fail closed when repository identity cannot be checked. The caller must retain normal enrollment and authenticated process-ancestry checks.

The implemented classification rules and execution-budget limits are owned by the [gate-model handbook](../src/content/docs/concepts/gate-model.md#recursive-run-containment).

## Scope

- Discover the canonical Git common directory before optional top-level attribution.
- Omit `--show-toplevel` for common directories that are not registered managed gates. Both identity commands select explicit bare discovery only from substantive metadata evidence, avoiding the general runners' basename-only bare heuristic for mixed ordinary project entries.
- Preserve positive managed-gate identity, including a managed bare repository without a worktree top level.
- Preserve active-step lookup and OS-authenticated peer ancestry for ordinary clones and proven nonrepository directories.
- Distinguish proven repository absence from cancellation, command failure, malformed metadata, dubious ownership, and output-drain failure.
- Apply a shared ten-second child context to identity Git commands. This bounds cancellable command execution, not uninterruptible filesystem calls, process-start syscalls, database waits, ancestry lookup, or the complete IPC handler.
- Update the canonical gate-model handbook and its area skill in the same change.

No enrollment rewrite, marker exception, empty-directory control request, raw mutation RPC, ancestry bypass, cache, retry loop, mirror reconciliation change, repository-config change, CI-contract change, generic CLI update, or peer-run mutation is included.

## Contract

The [gate-model handbook](../src/content/docs/concepts/gate-model.md#recursive-run-containment) owns the classification contract, supported HEAD forms, refusal conditions, and execution-budget limits. The [area skill](../../.agents/skills/gate-worktree-git-safety/SKILL.md) maps implementation owners and regressions; `git.CommandError` and the shell runner's comments own error-preservation details.

## Implementation order

1. Read classifier callers, Git command/error ownership, platform subprocess cleanup, existing topology tests, and the relevant area skills. No test-only production hook is needed.
2. Add distinct observable regressions before production changes. Record a failing run against the unchanged owner for each newly changed contract.
3. Add structured captured Git stderr without altering credentials redaction or normal error rendering. Preserve simultaneous wait/drain failures at the existing shell runner owner, and preserve underlying error identity through Git wrapping.
4. Stage common-directory proof before top-level lookup. Query the top level only for an already managed identity when active-run attribution needs it. A proven bare top-level absence retains positive nested proof. Every unexpected attribution failure returns the positive nested result plus an error, so it cannot become an ordinary admission.
5. Apply one shared ten-second identity-command context and propagate every unresolved lookup error. Keep the remainder of classification unchanged.
6. Update handbook and area skill; record actual red-to-green evidence.
7. Obtain one independent implementation review and resolve every in-scope finding before a pinned build.

## Required evidence

- A real ordinary clone with a separate small non-race Git helper that stalls only top-level lookup classifies successfully before the caller deadline. Assert the unnecessary lookup was not launched. Regression: `TestIdentityOrdinaryCloneNeverLaunchesTopLevel`.
- Cancellation or an expired deadline during the first identity read produces a matching error instead of `Nested=false`.
- Real malformed gitfiles, `.git` directories with missing/invalid HEAD, and corrupted bare metadata fail classification, including cases where Git emits an ordinary-discovery absence message or successfully discovers an enclosing repository. Successful discovery reports the common and actual Git directories in one command and is corroborated against physical ancestors, including gitfile targets. Bare metadata requires recognizable Git HEAD content or typed object/reference directory structure rather than isolated project filenames; substantive remnants still count when other components are missing, and an explicit `.git` entry remains significant. Ordinary files/directories named `HEAD`, `objects`, or `refs` remain allowed only after active-step and authenticated ancestry checks; an active descendant remains refused.
- A registered managed bare repository remains positively nested.
- Structured-error tests preserve captured redacted stderr, normal rendered error text, and underlying error identity. Unknown exit-128 stderr, dubious ownership, missing executables, canceled/deadline contexts, and drain errors all fail closed.
- A separate non-race helper prints the accepted absence diagnostic, exits 128, and leaves an isolated escaped stderr holder. Classification fails; `errors.As` retains the exit error and `errors.Is(err, exec.ErrWaitDelay)` survives. Fixture cleanup verifies and removes only its own escaped process.
- A child command without a shorter caller deadline is canceled by the identity execution budget, with supported process-group cleanup. A managed-attribution helper completes the first identity command and stalls the next; prove the ten-second budget is shared. Use isolated helpers and generous caller deadlines; prove execution cancellation rather than a brittle total-latency promise.
- Isolated daemon `gate_context` ingress and CLI preflight propagate identity failure and create no run or enrollment mutation. Test roots and processes never touch an installed daemon.
- Existing managed-worktree, symlinked-root, ordinary linked-worktree, independent/fork clone, forged-marker, authenticated-ancestry, concurrent-classification and no-mutation refusals pass.

Run the package owners first, then `make lint`, `go test -race ./...`, the applicable E2E suite through `scripts/e2e.sh` with its 2400-second package deadline, and a pinned build. Cross-platform compile/vet must cover the structured-error and classifier changes. Any broad baseline failure must be reproduced on the unchanged pin and reported; it does not permit weakening or deleting the failed assertion. Do not call a focused pass full-suite validation.

## Validation limits

Repository absence requires filesystem corroboration, and simultaneous command-exit and output-drain errors must remain visible. Error composition changes only at the existing lifecycle owner; process cleanup and grace policy remain unchanged. Supply red-first evidence for both contracts.

Behavioral validation must prove ordinary-clone classification, positive managed identity, and authenticated-descendant refusal through isolated fixtures. Successful classification in one snapshot does not prove that every intermittent timeout is resolved. Installation and daemon lifecycle changes are outside this source change.

Windows native runtime remains UNTESTED in this local review; no cross-platform runtime claim is made.
