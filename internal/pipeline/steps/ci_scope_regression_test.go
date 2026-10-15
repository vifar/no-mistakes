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

	// A run that recorded nothing falls back to the live configuration.
	if err := sctx.DB.SetRunCheckScope(sctx.Run.ID, ""); err != nil {
		t.Fatal(err)
	}
	reason, err = step.VerifyApprovalOverride(sctx)
	if err != nil || reason != "" {
		t.Fatalf("an unrecorded run must use the live configuration, got %q, %v", reason, err)
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
