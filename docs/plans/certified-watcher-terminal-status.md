# Certified watcher terminal status

The implemented contracts are owned by:

- [Push status reporting](../src/content/docs/reference/pipeline-steps.md#push):
  strict success proof, cancellation preservation, invalidation, and publication ownership.
- [Daemon lifecycle and recovery](../src/content/docs/concepts/daemon.md#crash-recovery):
  instance-bound planned shutdown, settlement retries, and recovery.

Behavioral coverage lives in `internal/db/status_validation_test.go`,
`internal/pipeline/terminal_settlement_test.go`,
`internal/pipeline/steps/commit_status_certificate_test.go`,
`internal/pipeline/steps/commit_status_terminal_race_test.go`, and the daemon
shutdown tests. `internal/e2e/certified_watcher_lifecycle_test.go` exercises
production gates and watcher cancellation with deterministic external agent and
forge fixtures; it is integration evidence, not live model or forge validation.
