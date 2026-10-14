# Gate-context Git latency mitigation

## Intent and evidence

Remove an unnecessary Git subprocess from ordinary-clone classification and fail closed when repository identity cannot be checked. The caller must retain normal enrollment and authenticated process-ancestry checks.

Observed repository-directory classification can exceed the client's 30-second reply budget while health remains responsive. Other snapshots, including concurrent ordinary-clone and nonrepository requests from one authenticated peer, complete within one second. The earlier slow syscall is unproven. This change removes a proven unnecessary dependency and contains Git execution; it must not be described as a confirmed diagnosis of every prior timeout or as a deadline for the complete classifier.

The implementation baseline is the installed revision d4036f9b2ad865119c49f97b7041affaad696c9e. Baseline `go test ./internal/gatecontext ./internal/git ./internal/shellenv ./internal/ipc` passed. Do not fold unrelated default-branch changes into a dedicated-runtime build.

## Scope

- Discover the canonical Git common directory before optional top-level attribution.
- Omit `--show-toplevel` for common directories that are not registered managed gates.
- Preserve positive managed-gate identity, including a managed bare repository without a worktree top level.
- Preserve active-step lookup and OS-authenticated peer ancestry for ordinary clones and proven nonrepository directories.
- Distinguish proven repository absence from cancellation, command failure, malformed metadata, dubious ownership, and output-drain failure.
- Apply a shared ten-second child context to identity Git commands. This bounds cancellable command execution, not uninterruptible filesystem calls, process-start syscalls, database waits, ancestry lookup, or the complete IPC handler.
- Update the canonical gate-model handbook and its area skill in the same change.

No enrollment rewrite, marker exception, empty-directory control request, raw mutation RPC, ancestry bypass, cache, retry loop, mirror reconciliation change, repository-config change, CI-contract change, generic CLI update, or peer-run mutation is included.

## Contract

1. The common-directory proof is authoritative only after canonical path, registered owner, and bare-repository structure agree.
2. An ordinary clone completes classification even if its top-level lookup would block. It does not execute that unnecessary lookup.
3. A managed gate remains nested even when top-level attribution is unavailable. Run and phase metadata are optional; they cannot determine refusal.
4. A canceled or failed first identity query returns an error. It never becomes a successful ordinary-caller verdict.
5. Proceeding without Git identity requires a narrowly recognized English ordinary-discovery absence diagnostic, a normal Git exit, complete output draining, and corroborating filesystem evidence that repository metadata is absent. Existing or malformed gitfiles, incomplete `.git` directories, corrupted bare metadata, and ambiguous discovery fail closed. Exit status 128 and stderr alone are insufficient. Do not parse a rendered error string or accept extra diagnostic lines.
6. Use a fixed English locale for these identity queries. Retain captured, redacted stderr in a typed Git command error, preserving normal error text and `errors.Is`/`errors.As` through `Unwrap`. Preserve simultaneous command-exit and output-drain failures; neither an exit code nor cancellation may hide `exec.ErrWaitDelay` or another drain failure.
7. Marker presence remains diagnostic. A daemon or active-agent descendant stays refused after changing to an ordinary clone or nonrepository directory.
8. All existing source, mirror, run, reference, and worktree records remain unchanged by classification.

## Implementation order

1. Read classifier callers, Git command/error ownership, platform subprocess cleanup, existing topology tests, and the relevant area skills. No test-only production hook is needed.
2. Add distinct observable regressions before production changes. Record a failing run against the unchanged owner for each newly changed contract.
3. Add structured captured Git stderr without altering credentials redaction or normal error rendering. Preserve simultaneous wait/drain failures at the existing shell runner owner, and preserve underlying error identity through Git wrapping.
4. Stage common-directory proof before top-level lookup. Query the top level only for an already managed identity when active-run attribution needs it. A proven bare top-level absence retains positive nested proof. Every unexpected attribution failure returns the positive nested result plus an error, so it cannot become an ordinary admission.
5. Apply one shared ten-second identity-command context and propagate every unresolved lookup error. Keep the remainder of classification unchanged.
6. Update handbook and area skill; record actual red-to-green evidence.
7. Obtain one independent implementation review and resolve every in-scope finding before a pinned build.

## Required evidence

- A real ordinary clone with a separate small non-race Git helper that stalls only top-level lookup classifies successfully before the caller deadline. Assert the unnecessary lookup was not launched. Existing tests do not inject this failure.
- Cancellation or an expired deadline during the first identity read produces a matching error instead of `Nested=false`.
- Real malformed gitfiles, `.git` directories with missing/invalid HEAD, and corrupted bare metadata fail classification, including cases where Git emits an ordinary-discovery absence message. A truly metadata-free directory remains allowed only after ancestry is checked; an active descendant remains refused.
- A registered managed bare repository remains positively nested.
- Structured-error tests preserve captured redacted stderr, normal rendered error text, and underlying error identity. Unknown exit-128 stderr, dubious ownership, missing executables, canceled/deadline contexts, and drain errors all fail closed.
- A separate non-race helper prints the accepted absence diagnostic, exits 128, and leaves an isolated escaped stderr holder. Classification fails; `errors.As` retains the exit error and `errors.Is(err, exec.ErrWaitDelay)` survives. Fixture cleanup verifies and removes only its own escaped process.
- A child command without a shorter caller deadline is canceled by the identity execution budget, with supported process-group cleanup. A managed-attribution helper completes the first identity command and stalls the next; prove the ten-second budget is shared. Use isolated helpers and generous caller deadlines; prove execution cancellation rather than a brittle total-latency promise.
- Isolated daemon `gate_context` ingress and CLI preflight propagate identity failure and create no run or enrollment mutation. Test roots and processes never touch an installed daemon.
- Existing managed-worktree, symlinked-root, ordinary linked-worktree, independent/fork clone, forged-marker, authenticated-ancestry, concurrent-classification and no-mutation refusals pass.

Run the package owners first, then `make lint`, `go test -race ./...`, the applicable E2E suite through `scripts/e2e.sh` with its 2400-second package deadline, and a pinned build. Cross-platform compile/vet must cover the structured-error and classifier changes. Any broad baseline failure must be reproduced on the unchanged pin and reported; it does not permit weakening or deleting the failed assertion. Do not call a focused pass full-suite validation.

## Rollout and recovery

Independent plan review required two corrections: absence needs filesystem corroboration, and simultaneous exit/drain errors must remain visible. Both are incorporated above. The second item changes only error composition at the existing lifecycle owner; process cleanup and grace policy remain unchanged. Implementation must supply red-first evidence for both corrected contracts before proceeding.

Prepare an exact-revision binary, verification receipt and backup before installation. Preserve existing shipping features and the dedicated root/executable pair. Do not run `make install`, which targets the generic executable and stops its daemon.

Re-read all active dedicated-root runs before a daemon replacement. A quiet point belongs to all owners, not one peer. Forced shutdown terminalizes tracked runs; it cannot preserve executing agents in place. Do not force a restart while owners are progressing. If an interruption is necessary, preserve consistent database/evidence/recovery refs and notify affected owners before the already-authorized supported lifecycle action.

Verify the new process owner and identity, responsive classification from an actual enrolled clone, and ordinary/managed/descendant refusal behavior through isolated integration evidence. The blocked owner resubmits only if no matching run exists. A healthy snapshot or an existing admitted run is not proof that the intermittent incident is permanently fixed.
