package steps

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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
	// A gate is raised by Execute, which records the scope it evaluated.
	if err := recordCIChecksScope(sctx); err != nil {
		t.Fatal(err)
	}
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

func TestCIScopeApprovalExternalOnlyReadiness(t *testing.T) {
	for _, tc := range []struct {
		name, protection, checks string
		noCI, ready              bool
	}{
		{"no_ci permits observed exclusions", "", `[{"name":"Greptile Review","state":"FAILURE","bucket":"fail","app":"greptile-apps"}]`, true, true},
		{"known required set permits exclusions", `{"contexts":["Greptile Review"]}`, `[{"name":"Greptile Review","state":"FAILURE","bucket":"fail","app":"greptile-apps"}]`, false, true},
		{"unknown required set waits", "", `[{"name":"Greptile Review","state":"FAILURE","bucket":"fail","app":"greptile-apps"}]`, false, false},
		{"unobserved rollup is not ready", "", `[]`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := append(fakeCIGH(t, "OPEN", tc.checks), greenGreptileCommentJSON, "FAKE_CLI_PROTECTION_JSON="+tc.protection, `FAKE_CLI_RULES_JSON=[]`)
			sctx, _, _, _, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
			sctx.Config.NoCI = tc.noCI
			sctx.Config.CI.ExternalChecks = []string{"Greptile Review"}
			if err := recordCIChecksScope(sctx); err != nil {
				t.Fatal(err)
			}
			reason, err := (&CIStep{}).VerifyApprovalOverride(sctx)
			if err != nil || (reason == "") != tc.ready {
				t.Fatalf("approval reason=%q err=%v want ready=%v", reason, err, tc.ready)
			}
		})
	}
}

func TestCIScopeAlwaysBotCommentsOnlyRespectExternalExclusions(t *testing.T) {
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
				if !external {
					if err != nil || outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, `"category":"ci-review-bot"`) || *ready {
						t.Fatalf("informational bot comments lost under always: %+v %v", outcome, err)
					}
					if failing && !strings.Contains(outcome.Findings, `"check":"build"`) {
						t.Fatalf("build failure lost beside bot comments: %+v", outcome)
					}
					if !failing {
						if reason, err := step.VerifyApprovalOverride(sctx); err != nil || reason == "" {
							t.Fatalf("informational comments approved cleanly: %q %v", reason, err)
						}
					}
				} else if failing {
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
	for _, mode := range []string{"informational", "external", "absent", "absent external"} {
		t.Run(mode, func(t *testing.T) {
			env := fakeCIGH(t, "OPEN", `[{"name":"build","state":"SUCCESS","bucket":"pass"},{"name":"optional","workflow":"Extra","state":"CANCELLED","bucket":"cancel"}]`)
			sctx, ag, _, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
			sctx.Env = append(sctx.Env, `FAKE_CLI_PROTECTION_JSON={"contexts":["build"]}`, `FAKE_CLI_RULES_JSON=[]`)
			if mode == "external" {
				sctx.Config.CI.MustPass = []string{"optional"}
				sctx.Config.CI.ExternalChecks = []string{"Extra / optional"}
			}
			if mode == "absent external" {
				sctx.Config.CI.MustPass = []string{"optional"}
				sctx.Config.CI.ExternalChecks = []string{"Extra / optional*"}
				sctx.Env = append(sctx.Env, `FAKE_CLI_PROTECTION_JSON={"contexts":["build","optional"]}`)
			}
			if mode == "absent" || mode == "absent external" {
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

func TestCIScopeRetainedAbsentBotIsDroppedWhenExternal(t *testing.T) {
	for _, registered := range []bool{false, true} {
		t.Run(map[bool]string{false: "absent", true: "registered"}[registered], func(t *testing.T) {
			initial := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
			env := append(fakeCIGH(t, "OPEN", initial), greenGreptileCommentJSON)
			sctx, ag, _, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
			step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
			observation, err := step.Execute(sctx)
			if err != nil || observation == nil || !observation.NeedsApproval {
				t.Fatalf("absent bot observation = %+v, %v", observation, err)
			}
			findings, err := types.ParseFindingsJSON(observation.Findings)
			if err != nil || len(findings.Items) != 1 {
				t.Fatalf("absent bot findings = %+v, %v", findings, err)
			}
			if item := findings.Items[0]; item.Check != "" || item.CheckApp != "greptile-apps" {
				t.Fatalf("absent bot lost app identity: %+v", item)
			}
			sctx.PreviousFindings = observation.Findings
			sctx.DeferredFindings = observation.Findings
			sctx.Fixing = true
			sctx.Config.CI.ExternalChecks = []string{"Greptile Review"}
			if registered {
				sctx.Env = append(sctx.Env, "FAKE_CLI_CHECKS="+greenGreptileChecksJSON)
			}
			marked := false
			sctx.MarkRunning = func() error {
				if sctx.PreviousFindings != "" || sctx.DeferredFindings != "" {
					t.Fatalf("retained external bot survived scoping: %s %s", sctx.PreviousFindings, sctx.DeferredFindings)
				}
				marked = true
				return nil
			}
			outcome, err := step.Execute(sctx)
			if !errors.Is(err, context.Canceled) || outcome != nil || !marked || !*ready || len(ag.calls) != 0 {
				t.Fatalf("external bot triggered repair or blocked monitoring: outcome=%+v err=%v marked=%v ready=%v calls=%d", outcome, err, marked, *ready, len(ag.calls))
			}
		})
	}
}

func TestCIScopeRetainedBotExclusionKeepsUnrelatedNamelessFindings(t *testing.T) {
	for _, excluded := range []bool{false, true} {
		policy := scm.CheckScopePolicy{RequiredChecks: scm.RequiredChecksAll}
		if excluded {
			policy.ExternalChecks = []string{"Greptile Review"}
		}
		sctx := &pipeline.StepContext{Ctx: context.Background()}
		encoded, err := types.MarshalFindingsJSON(Findings{Items: []Finding{
			{ID: "bot", Category: types.FindingCategoryCIReviewBot, CheckApp: "greptile-apps", Description: "bot comment"},
			{ID: "unrelated", Description: "unrelated concern"},
		}})
		if err != nil {
			t.Fatal(err)
		}
		sctx.PreviousFindings, sctx.DeferredFindings = encoded, encoded
		scope := scm.NewCheckScope(nil, policy, "main", nil)
		checks := []scm.Check{{Name: "Greptile Review", App: "greptile-apps", Bucket: scm.CheckBucketPass}}
		if err := scopeRetainedCIFindings(sctx, scope, &scm.PR{}, checks); err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{sctx.PreviousFindings, sctx.DeferredFindings} {
			got, err := types.ParseFindingsJSON(raw)
			if err != nil {
				t.Fatal(err)
			}
			if excluded {
				if len(got.Items) != 1 || got.Items[0].ID != "unrelated" {
					t.Fatalf("excluded bot or unrelated finding mishandled: %+v", got)
				}
			} else if len(got.Items) != 2 {
				t.Fatalf("included bot dropped: %+v", got)
			}
		}
	}
}

// An external pattern for a review bot's check must hold before the bot has
// registered one: otherwise the absent-bot fallback invents a nameless entry
// whose older comments block readiness.
func TestCIScopeAbsentExternalBotDoesNotReturnThroughComments(t *testing.T) {
	absentBot := `[{"name":"build","state":"SUCCESS","bucket":"pass"}]`
	for _, external := range []bool{true, false} {
		t.Run(map[bool]string{true: "external", false: "not external"}[external], func(t *testing.T) {
			env := append(fakeCIGH(t, "OPEN", absentBot), greenGreptileCommentJSON, `FAKE_CLI_PROTECTION_JSON={"contexts":["build"]}`, `FAKE_CLI_RULES_JSON=[]`)
			sctx, ag, _, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
			if external {
				sctx.Config.CI.ExternalChecks = []string{"Greptile Review"}
			}
			step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
			outcome, err := step.Execute(sctx)
			if external {
				if !errors.Is(err, context.Canceled) || outcome != nil || !*ready {
					t.Fatalf("an absent external bot blocked readiness: %+v %v", outcome, err)
				}
				if reason, err := step.VerifyApprovalOverride(sctx); err != nil || reason != "" {
					t.Fatalf("an absent external bot blocked approval: %q %v", reason, err)
				}
			} else if err != nil || outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "review bot") {
				t.Fatalf("an absent non-external bot lost today's behavior: %+v %v", outcome, err)
			}
			if len(ag.calls) != 0 {
				t.Fatal("repair dispatched")
			}
		})
	}
}

func TestCIScopeAlwaysRetainsInformationalBotComments(t *testing.T) {
	for _, named := range []bool{false, true} {
		sctx := &pipeline.StepContext{Ctx: context.Background(), Config: &config.Config{}}
		sctx.Config.CI.ReviewBotComments = config.CIReviewBotCommentsAlways
		item := Finding{ID: "bot", Category: types.FindingCategoryCIReviewBot, CheckApp: "greptile-apps", Description: "unresolved comment"}
		if named {
			item.Check = "Greptile Review"
		}
		encoded, err := types.MarshalFindingsJSON(Findings{Items: []Finding{item}})
		if err != nil {
			t.Fatal(err)
		}
		sctx.PreviousFindings, sctx.DeferredFindings = encoded, encoded
		scope := scm.NewCheckScope(&retainedBotRequiredHost{}, scm.CheckScopePolicy{}, "main", nil)
		checks := []scm.Check{{Name: "build", Bucket: scm.CheckBucketPass}, {Name: "Greptile Review", App: "greptile-apps", Bucket: scm.CheckBucketFail}}
		if err := scopeRetainedCIFindings(sctx, scope, &scm.PR{}, checks); err != nil {
			t.Fatal(err)
		}
		for _, raw := range []string{sctx.PreviousFindings, sctx.DeferredFindings} {
			got, err := types.ParseFindingsJSON(raw)
			if err != nil || len(got.Items) != 1 || got.Items[0].ID != "bot" {
				t.Fatalf("informational comment discarded under always: %+v %v", got, err)
			}
		}
	}
}

type retainedBotRequiredHost struct{ scm.Host }

func (*retainedBotRequiredHost) GetRequiredChecks(context.Context, *scm.PR, string) ([]scm.RequiredCheck, error) {
	return []scm.RequiredCheck{{Context: "build"}}, nil
}

// Approving a parked gate is judged under the scope the gate was raised with.
// A later config edit must not reclassify the original failure and turn the
// approval into a plain pass.
func TestCIScopeApprovalUsesTheScopeRecordedWhenTheGateWasRaised(t *testing.T) {
	env := append(fakeCIGH(t, "OPEN", `[{"name":"build","state":"FAILURE","bucket":"fail"},{"name":"lint","state":"SUCCESS","bucket":"pass"}]`),
		`FAKE_CLI_PROTECTION_JSON={"contexts":["build","lint"]}`, `FAKE_CLI_RULES_JSON=[]`)
	sctx, _, _, _, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
	step := &CIStep{}
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, `"check":"build"`) {
		t.Fatalf("the gate was not raised on the failing required check: %+v %v", outcome, err)
	}

	sctx.Config.CI.ExternalChecks = []string{"build"}
	reason, err := step.VerifyApprovalOverride(sctx)
	if err != nil || !strings.Contains(reason, "build") {
		t.Fatalf("approval after loosening external_checks = %q, %v; want the original failure recorded as an override", reason, err)
	}

	// A run that recorded nothing predates check scoping, so it is judged by
	// the legacy every-check policy like the strict receipt, never by the live
	// configuration, which here would now treat build as external.
	if err := sctx.DB.SetRunCheckScope(sctx.Run.ID, ""); err != nil {
		t.Fatal(err)
	}
	reason, err = step.VerifyApprovalOverride(sctx)
	if err != nil || !strings.Contains(reason, "build") {
		t.Fatalf("an unrecorded run must be judged on every check, got %q, %v", reason, err)
	}
}

// Under review_bot_comments: always a bot's comments are read after its check
// completes. A bot check outside the required scope that is still running keeps
// the head unsettled, so checks-passed cannot publish ahead of the review.
func TestCIScopePendingInformationalBotCheckBlocksReadinessUnderAlways(t *testing.T) {
	const build = `{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"}`
	const pendingBot = `{"name":"Greptile Review","state":"PENDING","bucket":"pending","app":"greptile-apps"}`
	const doneBot = `{"name":"Greptile Review","state":"SUCCESS","bucket":"pass","app":"greptile-apps"}`
	sequence := []string{"[" + build + "," + pendingBot + "]", "[" + build + "," + pendingBot + "]", "[" + build + "," + doneBot + "]"}
	env := append(fakeCIGHSequence(t, "OPEN", sequence), greenGreptileCommentJSON, `FAKE_CLI_PROTECTION_JSON={"contexts":["build"]}`, `FAKE_CLI_RULES_JSON=[]`)
	sctx, ag, _, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	polls := 0
	step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		if *ready {
			t.Fatal("checks-passed was published while the review bot's check was still running")
		}
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !strings.Contains(outcome.Findings, "api.test.ts") {
		t.Fatalf("outcome = %+v, %v; want the bot's comments read once its check completed", outcome, err)
	}
	if polls != 2 {
		t.Fatalf("polls before the comments were read = %d, want 2 (two pending observations)", polls)
	}
	if *ready || len(ag.calls) != 0 {
		t.Fatalf("ready=%v repairs=%d", *ready, len(ag.calls))
	}
}

func TestCIScopePendingExternalBotCheckDoesNotBlockReadiness(t *testing.T) {
	const checks = `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"},{"name":"Greptile Review","state":"PENDING","bucket":"pending","app":"greptile-apps"}]`
	env := append(fakeCIGH(t, "OPEN", checks), greenGreptileCommentJSON, `FAKE_CLI_PROTECTION_JSON={"contexts":["build"]}`, `FAKE_CLI_RULES_JSON=[]`)
	sctx, _, _, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	sctx.Config.CI.ExternalChecks = []string{"Greptile Review"}
	step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
	outcome, err := step.Execute(sctx)
	if !errors.Is(err, context.Canceled) || outcome != nil || !*ready {
		t.Fatalf("an external bot check blocked readiness: %+v %v ready=%v", outcome, err, *ready)
	}
}

func TestCIScopeCancelledInformationalBotCommentsParkUnderAlways(t *testing.T) {
	const checks = `[{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"},{"name":"Greptile Review","state":"CANCELLED","bucket":"cancel","app":"greptile-apps"}]`
	env := append(fakeCIGH(t, "OPEN", checks), greenGreptileCommentJSON, `FAKE_CLI_PROTECTION_JSON={"contexts":["build"]}`, `FAKE_CLI_RULES_JSON=[]`)
	sctx, ag, _, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable || *ready {
		t.Fatalf("cancelled informational bot comments must park: %+v %v ready=%v", outcome, err, *ready)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryCIReviewBot || findings.Items[0].Action != types.ActionAskUser || findings.Items[0].File != "src/commands/api.test.ts" {
		t.Fatalf("expected the unresolved bot comment: %+v %v", findings, err)
	}
	if reason, err := step.VerifyApprovalOverride(sctx); err != nil || !strings.Contains(reason, "need a decision") {
		t.Fatalf("approval over cancelled bot comments must record an override: %q %v", reason, err)
	}
	if len(ag.calls) != 0 {
		t.Fatal("repair dispatched")
	}
}

func TestCIScopePendingInformationalBotDoesNotDelayBuildRepair(t *testing.T) {
	const failedBuild = `{"name":"build","state":"FAILURE","bucket":"fail","app":"github-actions"}`
	const passedBuild = `{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions"}`
	const pendingBot = `{"name":"Greptile Review","state":"PENDING","bucket":"pending","app":"greptile-apps"}`
	const doneBot = `{"name":"Greptile Review","state":"SUCCESS","bucket":"pass","app":"greptile-apps"}`
	sequence := []string{
		"[" + failedBuild + "," + pendingBot + "]",
		"[" + failedBuild + "," + pendingBot + "]",
		"[" + failedBuild + "," + pendingBot + "]",
		"[" + passedBuild + "," + pendingBot + "]",
		"[" + passedBuild + "," + doneBot + "]",
	}
	env := append(fakeCIGHSequence(t, "OPEN", sequence), greenGreptileCommentJSON, `FAKE_CLI_PROTECTION_JSON={"contexts":["build"]}`, `FAKE_CLI_RULES_JSON=[]`)
	sctx, ag, _, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsAlways, env)
	polls := 0
	step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		if polls > 1 || *ready {
			t.Fatal("expected only the pending review to defer readiness after build repair")
		}
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.AutoFixable || polls != 0 {
		t.Fatalf("build failure must immediately reach repair: %+v %v polls=%d", outcome, err, polls)
	}
	findings, err := types.ParseFindingsJSON(outcome.Findings)
	if err != nil || len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryCICheck || findings.Items[0].Check != "build" || findings.Items[0].Action != types.ActionAutoFix {
		t.Fatalf("pending bot must not emit older comments beside build: %+v %v", findings, err)
	}
	sctx.Fixing = true
	sctx.PreviousFindings = outcome.Findings
	outcome, err = step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || outcome.AutoFixable || polls != 1 || len(ag.calls) != 1 || *ready {
		t.Fatalf("expected build repair then completed bot comments: %+v %v polls=%d repairs=%d ready=%v", outcome, err, polls, len(ag.calls), *ready)
	}
	findings, err = types.ParseFindingsJSON(outcome.Findings)
	if err != nil || len(findings.Items) != 1 || findings.Items[0].Category != types.FindingCategoryCIReviewBot || findings.Items[0].Action != types.ActionAskUser || findings.Items[0].File != "src/commands/api.test.ts" {
		t.Fatalf("completed bot must report its unresolved comment: %+v %v", findings, err)
	}
}

// A scope that cannot be recorded must stop the execution before any gate is
// raised: approval verification and the receipt read the record, and a stale
// one would judge a gate that was raised under a different policy.
func TestCIScopeUnrecordableScopeFailsTheExecutionBeforeAnyGate(t *testing.T) {
	env := append(fakeCIGH(t, "OPEN", `[{"name":"build","state":"FAILURE","bucket":"fail"}]`),
		`FAKE_CLI_PROTECTION_JSON={"contexts":["build"]}`, `FAKE_CLI_RULES_JSON=[]`)
	sctx, ag, _, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
	const stale = `{"required_checks":"all"}`
	if err := sctx.DB.SetRunCheckScope(sctx.Run.ID, stale); err != nil {
		t.Fatal(err)
	}
	step := &CIStep{recordCheckScope: func(*pipeline.StepContext) error { return errors.New("record CI check scope: disk full") }}
	outcome, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("Execute() = %+v, %v; want the persistence error", outcome, err)
	}
	if outcome != nil || *ready || len(ag.calls) != 0 {
		t.Fatalf("a gate or repair was produced under an unrecorded scope: outcome=%+v ready=%v repairs=%d", outcome, *ready, len(ag.calls))
	}
	if got, _ := sctx.DB.GetRunCheckScope(sctx.Run.ID); got != stale {
		t.Fatalf("recorded scope changed to %q", got)
	}
}

// A workflow rerun at the same commit can fail before it creates a job. The
// rollup keeps the older attempt's passing job, which must not satisfy the
// requirement the newer run now owns.
func TestCIScopeRerunFailingBeforeJobsBlocksAndIsRepaired(t *testing.T) {
	const checks = `[{"name":"build","state":"SUCCESS","bucket":"pass","workflow":"CI","app":"github-actions","link":"https://github.com/test/repo/actions/runs/1/job/5"}]`
	const runs = `FAKE_CLI_WORKFLOW_RUNS=[` +
		`{"id":1,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"success","run_started_at":"2026-10-14T10:00:00Z","updated_at":"2026-10-14T10:05:00Z"},` +
		`{"id":2,"workflow_id":10,"name":"CI","event":"pull_request","head_branch":"feature","status":"completed","conclusion":"failure","run_started_at":"2026-10-14T11:00:00Z","updated_at":"2026-10-14T11:00:05Z"}]`
	env := append(fakeCIGH(t, "OPEN", checks), runs, `FAKE_CLI_PROTECTION_JSON={"contexts":["build"]}`, `FAKE_CLI_RULES_JSON=[]`)
	sctx, _, logs, ready, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
	step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error { return context.Canceled })
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !outcome.AutoFixable || !strings.Contains(outcome.Findings, `"check":"CI"`) {
		t.Fatalf("the failed rerun was not offered for repair: %+v %v logs=%v", outcome, err, *logs)
	}
	if *ready {
		t.Fatal("the stale passing job published readiness")
	}
}

// The target a PR points at decides which checks are required of it. After a
// retarget mid-monitoring, the new base's requirement must block and the
// failure only the old base required must not be dispatched.
func TestCIScopeFollowsAPRRetargetedWhileMonitoring(t *testing.T) {
	const pending = `[{"name":"old-only","state":"PENDING","bucket":"pending"},{"name":"new-req","state":"PENDING","bucket":"pending"}]`
	const failed = `[{"name":"old-only","state":"FAILURE","bucket":"fail"},{"name":"new-req","state":"FAILURE","bucket":"fail"}]`
	baseFile := filepath.Join(t.TempDir(), "pr-base")
	if err := os.WriteFile(baseFile, []byte("main"), 0o644); err != nil {
		t.Fatal(err)
	}
	env := append(fakeCIGHSequence(t, "OPEN", []string{pending, failed}),
		"FAKE_CLI_PR_BASE_FILE="+baseFile,
		`FAKE_CLI_PROTECTION_JSON_MAIN={"contexts":["old-only"]}`,
		`FAKE_CLI_PROTECTION_JSON_RELEASE={"contexts":["new-req"]}`,
		`FAKE_CLI_RULES_JSON=[]`)
	sctx, ag, logs, _, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
	sctx.PRTarget = nil
	step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		return os.WriteFile(baseFile, []byte("release"), 0o644)
	})
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, %v; want the new base's failing requirement to park the step. logs: %v", outcome, err, *logs)
	}
	if !strings.Contains(outcome.Findings, `"check":"new-req"`) || strings.Contains(outcome.Findings, "old-only") {
		t.Fatalf("findings %s must name only the new base's requirement", outcome.Findings)
	}
	if len(ag.calls) != 0 {
		t.Fatalf("repair dispatched before the park: %d", len(ag.calls))
	}
}

// The target a PR points at decides which checks it requires. A retarget
// while the step is monitoring must re-scope every later poll: the new base's
// required failure blocks, and a failure only the old base required is not
// dispatched.
func retargetFixture(t *testing.T) (*pipeline.StepContext, string) {
	t.Helper()
	baseFile := filepath.Join(t.TempDir(), "base")
	if err := os.WriteFile(baseFile, []byte("main"), 0o644); err != nil {
		t.Fatal(err)
	}
	pending := `[{"name":"old-only","state":"PENDING","bucket":"pending"},{"name":"new-req","state":"PENDING","bucket":"pending"}]`
	failed := `[{"name":"old-only","state":"FAILURE","bucket":"fail"},{"name":"new-req","state":"FAILURE","bucket":"fail"}]`
	env := append(fakeCIGHSequence(t, "OPEN", []string{pending, failed}),
		"FAKE_CLI_PR_BASE_FILE="+baseFile,
		`FAKE_CLI_PROTECTION_JSON_MAIN={"contexts":["old-only"]}`,
		`FAKE_CLI_PROTECTION_JSON_RELEASE={"contexts":["new-req"]}`,
		`FAKE_CLI_RULES_JSON=[]`)
	sctx, _, _, _, _ := newGreenReviewBotContext(t, config.CIReviewBotCommentsOnFailure, env)
	return sctx, baseFile
}

func TestCIScopeRetargetMidMonitoringReclassifiesBeforeDispatch(t *testing.T) {
	sctx, baseFile := retargetFixture(t)
	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }
	polls := 0
	step := (&CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		polls++
		return os.WriteFile(baseFile, []byte("release"), 0o644)
	})
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("outcome = %+v, %v; want the new base's failure raised", outcome, err)
	}
	if !strings.Contains(outcome.Findings, `"check":"new-req"`) || strings.Contains(outcome.Findings, `"check":"old-only"`) {
		t.Fatalf("findings %s must name the new base's required check only", outcome.Findings)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "PR target changed from main to release") {
		t.Fatalf("the retarget was not reported: %v", logs)
	}
}

func TestCIScopeRetargetFailsWhenTheScopeCannotBeRecorded(t *testing.T) {
	sctx, baseFile := retargetFixture(t)
	records := 0
	step := (&CIStep{recordCheckScope: func(*pipeline.StepContext) error {
		records++
		if records > 1 {
			return errors.New("injected record failure")
		}
		return nil
	}}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		return os.WriteFile(baseFile, []byte("release"), 0o644)
	})
	outcome, err := step.Execute(sctx)
	if err == nil || !strings.Contains(err.Error(), "injected record failure") || outcome != nil {
		t.Fatalf("a retarget that cannot be recorded must fail the step: %+v, %v", outcome, err)
	}
}
