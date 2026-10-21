# Review fix verification

R1: Apply bounded PR-context read retries to published-target retention's shared protection and mergeability checks, retaining typed GitHub keyring timeout diagnostics.

Command:

```sh
go test -p 4 ./internal/pipeline ./internal/pipeline/steps ./internal/scm/github -run 'TestIssue100|TestRetainedTargetObservationContinuity|TestQualifiedPublishedHeadTargetAdvanceDoesNotMoveHead|TestRequiresUpToDateUsesAllProtectionSources|TestGetRequiredChecks|TestParseMergeableState' -count=1 -timeout 5m
```

Result: PASS (exit 0).

```text
ok  github.com/kunchenguid/no-mistakes/internal/pipeline         3.374s
ok  github.com/kunchenguid/no-mistakes/internal/pipeline/steps   49.782s
ok  github.com/kunchenguid/no-mistakes/internal/scm/github       0.640s
```

New behavioral coverage exercises the real fake-gh process through published-target retention for classic protection, branch rules, and mergeability: timeout then success, three-attempt exhaustion with real backoff, and bad credentials without retry. It checks the original comparison and completed review approval remain unchanged, records retention only on success, and checks the published failure description on exhaustion. A separate adapter regression checks mergeability error classification and stderr diagnostics.

No pre-fix red run was performed: this fix round required all edits before one focused verification. No full-suite, lint, build, publication, or CI phase was run. Earlier broad-suite evidence remains subject to the aggregate timeout limitations recorded in the supplied driver context; this focused pass does not establish a clean full-suite pass.
