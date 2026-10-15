package steps

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// strictCIRun drives one CI step execution of a strict full-shipping run whose
// head carries checksJSON, with the target branch protected as protection says.
func strictCIRun(t *testing.T, checksJSON, protection string, ci config.CI, noCI ...bool) (*pipeline.StepOutcome, error, []string, *db.Run, string) {
	t.Helper()
	dir, base, head := setupGitRepo(t)
	remote := t.TempDir()
	gitCmd(t, remote, "init", "--bare")
	gitCmd(t, dir, "push", remote, "main")
	url := "https://github.com/test/repo.git"
	gitCmd(t, dir, "config", "url."+remote+".insteadOf", url)

	sctx := newTestContextWithDBRecords(t, &mockAgent{name: "test"}, dir, base, head, config.Commands{})
	sctx.Repo.UpstreamURL = url
	run, err := sctx.DB.InsertRunWithShipping(sctx.Repo.ID, "feature", head, base, nil, "", "", "", "main", false, nil, &types.ShippingOptions{Mode: types.ShippingFull, TargetBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	sctx.Run = run
	comparison, err := sctx.DB.CaptureReviewComparison(run.ID, db.ReviewComparison{SourceRepo: "test/repo", SourceBranch: "feature", TargetBranch: "main", TargetSHA: base, HeadSHA: head, MergeBaseSHA: base, DiffDigest: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatal(err)
	}
	review, err := sctx.DB.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if err := sctx.DB.CompleteReviewStep(review.ID, run.ID, head, 0, 1, "", comparison.Generation); err != nil {
		t.Fatal(err)
	}
	prURL := "https://github.com/test/repo/pull/42"
	if err := sctx.DB.UpdateRunPRURL(run.ID, prURL); err != nil {
		t.Fatal(err)
	}
	run.PRURL = &prURL

	sctx.Env = append(fakeCIGH(t, "OPEN", checksJSON),
		"FAKE_CLI_PR_HEAD_SHA="+head,
		"FAKE_CLI_PR_BASE=main",
		"FAKE_CLI_PROTECTION_JSON="+protection,
		"FAKE_CLI_RULES_JSON=[]",
	)
	sctx.Config.CITimeout = 10 * time.Minute
	sctx.Config.CI = ci
	if len(noCI) > 0 {
		sctx.Config.NoCI = noCI[0]
	}
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	step := (&CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	})
	outcome, err := step.Execute(sctx)
	durable, getErr := sctx.DB.GetRun(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	scope, getErr := sctx.DB.GetRunCheckScope(run.ID)
	if getErr != nil {
		t.Fatal(getErr)
	}
	return outcome, err, logs, durable, scope
}

func TestStrictCIExternalOnlyReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, checks, protection string
		noCI, ready              bool
	}{
		{"declared no_ci with observed external check", `[{"name":"approval-status","state":"FAILURE","bucket":"fail"}]`, "", true, true},
		{"known required set all external", `[{"name":"approval-status","state":"FAILURE","bucket":"fail"}]`, `{"contexts":["approval-status"]}`, false, true},
		{"unknown required set waits for real CI", `[{"name":"approval-status","state":"FAILURE","bucket":"fail"}]`, "", false, false},
		{"declared no_ci still needs observed checks in strict shipping", `[]`, "", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			outcome, err, logs, run, scope := strictCIRun(t, tc.checks, tc.protection, config.CI{ExternalChecks: []string{"approval-status"}}, tc.noCI)
			if !errors.Is(err, context.Canceled) || outcome != nil {
				t.Fatalf("Execute=%+v %v logs=%v", outcome, err, logs)
			}
			if (run.CIReadyAt != nil) != tc.ready || run.CIReadyNoCI {
				t.Fatalf("ready=%v declared-empty=%v want=%v logs=%v", run.CIReadyAt, run.CIReadyNoCI, tc.ready, logs)
			}
			if tc.noCI && !strings.Contains(scope, `"no_ci":true`) {
				t.Fatalf("trusted no_ci not recorded: %s", scope)
			}
		})
	}
}

const strictChecks = `[{"name":"build","state":"SUCCESS","bucket":"pass"},` +
	`{"name":"approve","state":"FAILURE","bucket":"fail","workflow":"Final Approval"},` +
	`{"name":"approval-status","state":"FAILURE","bucket":"fail"}]`

// An external approval app can only approve a head that was published and
// reviewed, so a strict run that parked on its failing contexts could never
// reach the review that would turn them green.
func TestStrictCIReachesReadyWhileExternalApprovalChecksAreRed(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		protection string
		ci         config.CI
	}{
		"not required by the target branch": {`{"contexts":["build"]}`, config.CI{}},
		"required but declared external":    {`{"contexts":["build","approve","approval-status"]}`, config.CI{ExternalChecks: []string{"Final Approval / approve*", "approval-status"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			outcome, err, logs, run, _ := strictCIRun(t, strictChecks, tc.protection, tc.ci)
			if !errors.Is(err, context.Canceled) || outcome != nil {
				t.Fatalf("Execute() = %+v, %v; want a monitor that kept running. logs: %v", outcome, err, logs)
			}
			if run.CIReadyAt == nil || run.CIReadyNoCI {
				t.Fatalf("strict run did not reach ci_ready: %+v; logs: %v", run, logs)
			}
			found := false
			for _, l := range logs {
				found = found || l == cimonitor.ChecksPassedMsg
			}
			if !found {
				t.Fatalf("checks-passed was not reported; logs: %v", logs)
			}
		})
	}
}

func TestStrictCIStillParksOnAFailingRequiredCheck(t *testing.T) {
	t.Parallel()
	failing := strings.Replace(strictChecks, `{"name":"build","state":"SUCCESS","bucket":"pass"}`, `{"name":"build","state":"FAILURE","bucket":"fail"}`, 1)
	outcome, err, logs, run, _ := strictCIRun(t, failing, `{"contexts":["build"]}`, config.CI{})
	if err != nil || outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, `"check":"build"`) {
		t.Fatalf("Execute() = %+v, %v; want a finding for build. logs: %v", outcome, err, logs)
	}
	if strings.Contains(outcome.Findings, "approve") {
		t.Fatalf("findings name the external approval checks: %s", outcome.Findings)
	}
	if run.CIReadyAt != nil {
		t.Fatalf("a failing required check left the run ci_ready: %+v", run)
	}
}

func TestStrictCIWithoutScopeParksOnEveryFailingCheck(t *testing.T) {
	t.Parallel()
	outcome, err, logs, run, _ := strictCIRun(t, strictChecks, "", config.CI{RequiredChecks: "all"})
	if err != nil || outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, `"check":"approve"`) {
		t.Fatalf("Execute() = %+v, %v; want ci.required_checks: all to keep today's behavior. logs: %v", outcome, err, logs)
	}
	if run.CIReadyAt != nil {
		t.Fatalf("run is ci_ready: %+v", run)
	}
}

func TestCIStepRecordsTheScopeItEvaluatedUnder(t *testing.T) {
	t.Parallel()
	ci := config.CI{RequiredChecks: "auto", MustPass: []string{"lint*"}, ExternalChecks: []string{"approve"}}
	_, _, _, _, scope := strictCIRun(t, strictChecks, `{"contexts":["build"]}`, ci)
	// The record carries the required set the scope resolved, so approval and
	// the receipt rebuild exactly this scope.
	want := `{"required_checks":"auto","must_pass":["lint*"],"external_checks":["approve"],"resolved":{"required":[{"context":"build"}],"derived":true}}`
	if scope != want {
		t.Fatalf("recorded scope = %s, want %s", scope, want)
	}
}
