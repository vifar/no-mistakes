package citest

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
	"github.com/kunchenguid/no-mistakes/internal/scm"
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
	t.Run("collector excludes the owned pending status", func(t *testing.T) {
		dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
		sctx := stepstest.NewTestContext(t, &stepstest.MockAgent{AgentName: "test"}, dir, baseSHA, headSHA, config.Commands{})
		sctx.Env = stepstest.FakeCIGH(t, "OPEN", approvalDeadlockChecks)
		host, skip := steps.BuildHostForTest(sctx, scm.ProviderGitHub)
		if host == nil {
			t.Fatalf("buildHost returned nil: %s", skip)
		}
		checks, err := host.GetChecks(context.Background(), &scm.PR{Number: "42", HeadSHA: headSHA})
		if err != nil {
			t.Fatal(err)
		}
		want := []scm.Check{
			{Name: "actionlint", ProviderID: "github-check-run:1", HeadSHA: headSHA, Kind: scm.CheckKindRun, State: "SUCCESS", Bucket: scm.CheckBucketPass, App: "github-actions", AppID: 15368},
			{Name: "conflict-markers", ProviderID: "github-check-run:2", HeadSHA: headSHA, Kind: scm.CheckKindRun, State: "SUCCESS", Bucket: scm.CheckBucketPass, App: "github-actions", AppID: 15368},
			{Name: "approve #8521", ProviderID: "github-check-run:3", HeadSHA: headSHA, Kind: scm.CheckKindRun, State: "IN_PROGRESS", Bucket: scm.CheckBucketPending, App: "github-actions", AppID: 15368, Workflow: "PR Final Approval"},
			{Name: "approval-status", ProviderID: "github-check-run:4", HeadSHA: headSHA, Kind: scm.CheckKindRun, State: "IN_PROGRESS", Bucket: scm.CheckBucketPending},
		}
		if !reflect.DeepEqual(checks, want) {
			t.Fatalf("collected checks = %+v, want only approval and validation CheckRuns: %+v", checks, want)
		}
	})

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
			summary: "informational (not required, ignored): approve #8521 (pending)",
		},
		{
			name: "required actionlint failure remains actionable", protection: requiredApproval, ci: external, failLint: true,
			summary: "external (awaiting outside approval): approval-status (pending), approve #8521 (pending)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checks := approvalDeadlockChecks
			if tc.failLint {
				checks = strings.Replace(checks, `"name":"actionlint","status":"COMPLETED","conclusion":"SUCCESS"`, `"name":"actionlint","status":"COMPLETED","conclusion":"FAILURE"`, 1)
			}
			r := runScoped(t, []string{checks}, []string{tc.protection}, tc.ci, 1)
			var summaries []string
			for _, line := range r.logs {
				if strings.HasPrefix(line, "CI checks not evaluated: ") {
					summaries = append(summaries, line)
				}
			}
			if want := []string{"CI checks not evaluated: " + tc.summary}; !slices.Equal(summaries, want) {
				t.Errorf("scope summaries = %q, want %q (owned pending status must be absent)", summaries, want)
			}
			if r.logged("no-mistakes (pending)") {
				t.Error("owned pending status leaked into the CI summary")
			}
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
