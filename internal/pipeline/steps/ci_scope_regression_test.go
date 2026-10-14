package steps

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/scm"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestCIScopeApprovalUsesSelectedTarget(t *testing.T) {
	dir, base, head := setupGitRepo(t)
	gitCmd(t, dir, "branch", "release", base)
	env, logFile := fakeCIGHLoggedSequence(t, "OPEN", []string{`[{"name":"build","state":"SUCCESS"},{"name":"deploy","state":"FAILURE"}]`}, "", "")
	sctx := newTestContextWithDBRecords(t, nil, dir, base, head, config.Commands{})
	sctx.Env = append(env, `FAKE_CLI_PROTECTION_JSON={"contexts":["build","deploy"]}`, `FAKE_CLI_RULES_JSON=[]`)
	bindExistingPRMutationFixture(t, sctx, "https://github.com/test/repo/pull/42", "release")
	reason, err := (&CIStep{}).VerifyApprovalOverride(sctx)
	if err != nil || !strings.Contains(reason, "deploy") {
		t.Fatalf("reason=%q err=%v", reason, err)
	}
	calls, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "/branches/release/protection/required_status_checks") || strings.Contains(string(calls), "/branches/main/protection/required_status_checks") {
		t.Fatalf("wrong required-check target: %s", calls)
	}
	sctx.PRTarget.TargetBranch = ""
	reason, err = (&CIStep{}).VerifyApprovalOverride(sctx)
	if err != nil || !strings.Contains(reason, "could not verify live CI target") {
		t.Fatalf("unreadable target passed: %q %v", reason, err)
	}
}

func TestCIScopeExcludedBotCannotReturnThroughComments(t *testing.T) {
	for _, external := range []bool{false, true} {
		for _, failing := range []bool{false, true} {
			t.Run(strings.Join([]string{map[bool]string{true: "external", false: "informational"}[external], map[bool]string{true: "red", false: "green"}[failing]}, "/"), func(t *testing.T) {
				checks := strings.ReplaceAll(greenGreptileChecksJSON, `"name":"Greptile Review","state":"SUCCESS","bucket":"pass"`, `"name":"Greptile Review","state":"FAILURE","bucket":"fail"`)
				if failing {
					checks = strings.ReplaceAll(checks, `"name":"build","state":"SUCCESS","bucket":"pass"`, `"name":"build","state":"FAILURE","bucket":"fail"`)
				}
				env := append(fakeCIGH(t, "OPEN", checks), greenGreptileCommentJSON, `FAKE_CLI_PROTECTION_JSON={"contexts":["build"]}`, `FAKE_CLI_RULES_JSON=[]`)
				sctx, ag, _, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
				if external {
					sctx.Config.CI.ExternalChecks = []string{"Greptile Review"}
					sctx.Env = append(sctx.Env, `FAKE_CLI_PROTECTION_JSON={"contexts":["build","Greptile Review"]}`)
				}
				step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
				outcome, err := step.Execute(sctx)
				if failing {
					if err != nil || outcome == nil || strings.Contains(outcome.Findings, "greptile") || strings.Contains(outcome.Findings, "Greptile") || !strings.Contains(outcome.Findings, `"check":"build"`) {
						t.Fatalf("excluded bot resurfaced: %+v %v", outcome, err)
					}
				} else {
					if !errors.Is(err, context.Canceled) || outcome != nil || !*ready {
						t.Fatalf("excluded bot blocked readiness: %+v %v", outcome, err)
					}
					reason, err := step.VerifyApprovalOverride(sctx)
					if err != nil || reason != "" {
						t.Fatalf("excluded bot blocked approval: %q %v", reason, err)
					}
				}
				if len(ag.calls) != 0 {
					t.Fatal("excluded bot dispatched repair")
				}
			})
		}
	}
}

func TestCIScopeRecoveredWorkDropsExcludedChecksAndKeepsBudget(t *testing.T) {
	for _, mode := range []string{"informational", "external", "absent"} {
		t.Run(mode, func(t *testing.T) {
			env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"optional","workflow":"Extra","state":"CANCELLED","bucket":"cancel"}]`)
			sctx, ag, _, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
			sctx.Env = append(sctx.Env, `FAKE_CLI_PROTECTION_JSON={"contexts":["build"]}`, `FAKE_CLI_RULES_JSON=[]`)
			if mode == "external" {
				sctx.Config.CI.MustPass = []string{"optional"}
				sctx.Config.CI.ExternalChecks = []string{"Extra / optional"}
			}
			if mode == "absent" {
				sctx.Env = append(sctx.Env, `FAKE_CLI_CHECKS=[{"name":"build","state":"SUCCESS","bucket":"pass"}]`)
			}
			sctx.Fixing = true
			result, err := sctx.DB.InsertStepResult(sctx.Run.ID, types.StepCI)
			if err != nil {
				t.Fatal(err)
			}
			sctx.StepResultID = result.ID
			if err := sctx.DB.UpdateStepStatus(result.ID, types.StepStatusFixing); err != nil {
				t.Fatal(err)
			}
			marked := 0
			sctx.MarkRunning = func() error {
				marked++
				return sctx.DB.UpdateStepStatus(result.ID, types.StepStatusRunning)
			}
			sctx.PreviousFindings = `{"findings":[{"check":"optional","check_id":"previous-execution","category":"ci-check","action":"auto-fix","description":"old failure"}]}`
			sctx.DeferredFindings = sctx.PreviousFindings
			budget := checkRerunBudget{}
			budget.spend(scm.Check{Name: "optional", Bucket: scm.CheckBucketCancel}, nil, sctx.Run.HeadSHA)
			encoded, err := budget.marshal()
			if err != nil {
				t.Fatal(err)
			}
			if err := sctx.DB.SetRunCIRerunState(sctx.Run.ID, encoded); err != nil {
				t.Fatal(err)
			}
			step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
			outcome, err := step.Execute(sctx)
			if !errors.Is(err, context.Canceled) || outcome != nil || !*ready || len(ag.calls) != 0 {
				t.Fatalf("retained excluded work blocked green checks: %+v %v calls=%d", outcome, err, len(ag.calls))
			}
			stored, err := sctx.DB.GetStepResult(result.ID)
			if err != nil {
				t.Fatal(err)
			}
			if marked != 1 || stored.Status != types.StepStatusRunning {
				t.Fatalf("monitor resumed with status %s and %d running transitions", stored.Status, marked)
			}
			if sctx.PreviousFindings != "" || sctx.DeferredFindings != "" {
				t.Fatalf("excluded findings retained: %s %s", sctx.PreviousFindings, sctx.DeferredFindings)
			}
			durable, err := sctx.DB.GetRunCIRerunState(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			recovered := checkRerunBudget{}
			if err := recovered.unmarshal(durable); err != nil {
				t.Fatal(err)
			}
			if recovered.used("optional") != 1 || len(recovered.rollup) != 0 {
				t.Fatalf("retained state: %+v", recovered)
			}
			if recovered.remaining("optional", 1) != 0 {
				t.Fatal("excluded rerun regained spent budget")
			}
		})
	}
}

func TestCIStep_EmptyRepairPropagatesMarkRunningFailure(t *testing.T) {
	markErr := errors.New("persist running status")
	sctx := &pipeline.StepContext{
		Log:         func(string) {},
		MarkRunning: func() error { return markErr },
	}
	outcome, err := (&CIStep{}).repairFromFindings(sctx, nil, nil)
	if outcome != nil || !errors.Is(err, markErr) {
		t.Fatalf("outcome = %+v err = %v, want running transition failure", outcome, err)
	}
}
