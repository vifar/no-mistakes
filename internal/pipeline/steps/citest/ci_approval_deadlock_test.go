package citest

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
)

// The approval app waits for the pipeline's commit status to leave pending.
// Keep it a StatusContext, not a same-named CheckRun: collection excludes only
// the owned status, and scope must not recreate it as a missing requirement.
const approvalDeadlockChecks = `{"data":{"repository":{"object":{"statusCheckRollup":{"contexts":{"nodes":[` +
	`{"__typename":"StatusContext","id":"pipeline-status","context":"no-mistakes","state":"PENDING"},` +
	`{"__typename":"CheckRun","databaseId":1,"name":"actionlint","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"app":{"slug":"github-actions","databaseId":15368}}},` +
	`{"__typename":"CheckRun","databaseId":2,"name":"conflict-markers","status":"COMPLETED","conclusion":"SUCCESS","checkSuite":{"app":{"slug":"github-actions","databaseId":15368}}},` +
	`{"__typename":"CheckRun","databaseId":3,"name":"approve #8521","status":"IN_PROGRESS","checkSuite":{"app":{"slug":"github-actions","databaseId":15368},"workflowRun":{"workflow":{"name":"PR Final Approval"}}}},` +
	`{"__typename":"CheckRun","databaseId":4,"name":"approval-status","status":"IN_PROGRESS"}` +
	`],"pageInfo":{"hasNextPage":false}}}}}}}`

const approvalDeadlockProtection = `FAKE_CLI_PROTECTION_JSON={"contexts":["no-mistakes","actionlint","conflict-markers"]}`

func TestCIStep_ApprovalStatusDeadlock(t *testing.T) {
	const requiredApproval = `FAKE_CLI_PROTECTION_JSON={"contexts":["no-mistakes","actionlint","conflict-markers","approval-status"]}`
	external := config.CI{ExternalChecks: []string{"approval-status", "approve #*"}}
	for _, tc := range []struct {
		name       string
		protection string
		ci         config.CI
		failLint   bool
		ready      bool
		summary    string
	}{
		{
			name: "auto ignores pending approval checks and owned status", protection: approvalDeadlockProtection,
			ready: true, summary: "informational (not required, ignored): approval-status (pending), approve #8521 (pending)",
		},
		{
			name: "required approval status explicitly external", protection: requiredApproval, ci: external,
			ready: true, summary: "external (awaiting outside approval): approval-status (pending), approve #8521 (pending)",
		},
		{
			name: "required approval status without exclusion waits", protection: requiredApproval,
		},
		{
			name: "required actionlint failure remains actionable", protection: requiredApproval, ci: external, failLint: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := approvalDeadlockChecks
			if tc.failLint {
				checks = strings.Replace(checks, `"name":"actionlint","status":"COMPLETED","conclusion":"SUCCESS"`, `"name":"actionlint","status":"COMPLETED","conclusion":"FAILURE"`, 1)
			}
			r := runScoped(t, []string{checks}, []string{tc.protection}, tc.ci, 1)
			if tc.ready {
				assertReady(t, r)
				if !r.logged(tc.summary) {
					t.Fatalf("approval checks lost their scope decision %q; logs: %v", tc.summary, r.logs)
				}
			} else if tc.failLint {
				assertBlockedOn(t, r, "actionlint", "no-mistakes", "approval-status", "approve #8521")
				if !r.outcome.AutoFixable {
					t.Fatalf("required actionlint failure is not actionable: %+v", r.outcome)
				}
			} else {
				if !errors.Is(r.err, context.Canceled) || r.outcome != nil || r.reportedPassed() || r.ciReady {
					t.Fatalf("required pending approval-status must keep waiting: outcome=%+v err=%v ready=%v logs=%v", r.outcome, r.err, r.ciReady, r.logs)
				}
			}
			if len(r.agent.Calls) != 0 {
				t.Fatalf("observation dispatched %d repairs", len(r.agent.Calls))
			}
			if got := r.ghCalls("/branches/main/protection/required_status_checks"); got != 1 {
				t.Fatalf("target requirements read %d times, want once; calls: %s", got, r.ghLog)
			}
		})
	}
}
