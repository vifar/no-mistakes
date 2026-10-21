# Issue 60 review repair

Toolchain: Go 1.27.3, darwin/arm64. All test commands use `-p 4`, without `-race`.

## Before the repair

```sh
/opt/homebrew/bin/go test -p 4 ./internal/daemon -run 'TestSetupFailurePreservesCancellation/(inline|queued)/abort|TestEndingSupersessionNamesNonPushTrigger' -count=1 -timeout 3m
```

Exit 1. Inline setup cancellation saved `cancelled` without an ending event. All four rerun/fresh, queued/admitted cases recorded `push` with invented received SHA/ref. Queued setup cancellation passed. Full output: [review-60-red.log](review-60-red.log).

## After the repair

```sh
/opt/homebrew/bin/go test -p 4 ./internal/daemon -run 'TestEnding|TestSetupFailurePreservesCancellation|TestSetupFailureProvenanceSurvivesPublicationDelay|TestClaimedCancellationDoesNotRequireDatabaseAccess|TestClaimedCancelRPCStopsWorkWhenDatabaseReadsFail|TestClaimedCancellationWaitsForAgentExit|TestSupersedeOfClaimedQueuedAndFinishingRuns|TestQueueCancellationWriteFailurePreservesOwnership' -count=1 -timeout 5m
```

Exit 0; focused daemon regressions and controls passed. Output: [review-60-green.log](review-60-green.log).

The issue 60 attribution contract is the independent oracle: retain the actual stop caller/reason and replacement trigger, without invented received-push provenance. The delayed-settlement cases also retain existing independent-failure and shutdown outcomes. This review phase runs only focused daemon tests; broader Test, lint and publication remain owned by the outer executor. Evidence stays inside this worktree to honor the workspace boundary.
