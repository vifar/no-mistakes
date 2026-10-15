package citest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/agent"
	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

type scopeRun struct {
	outcome *pipeline.StepOutcome
	err     error
	logs    []string
	ghLog   string
	agent   *stepstest.MockAgent
	ciReady bool
}

func (r scopeRun) logged(substr string) bool {
	for _, l := range r.logs {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

func (r scopeRun) reportedPassed() bool { return r.logged(cimonitor.ChecksPassedMsg) }

func (r scopeRun) ghCalls(substr string) int {
	return strings.Count(r.ghLog, substr)
}

// runScoped executes the CI step against a fake GitHub whose check rollup
// advances through checks (one entry per poll). env adds the fake protection and
// rules answers. The monitor is stopped after maxPolls waits, so a run that
// never settles ends in context.Canceled.
func runScoped(t *testing.T, checks []string, env []string, ci config.CI, maxPolls int) scopeRun {
	t.Helper()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ghEnv, logFile := stepstest.FakeCIGHLoggedSequence(t, "OPEN", checks, "", "")
	prURL := "https://github.com/test/repo/pull/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(ghEnv, "FAKE_CLI_RULES_JSON=[]", "FAKE_CLI_PROTECTION_JSON={}")
	sctx.Env = append(sctx.Env, env...)
	stepstest.BindCurrentPRComparison(t, sctx, prURL, "test/repo", "main")
	sctx.Config.CITimeout = 10 * time.Minute
	sctx.Config.AutoFix.CI = 3
	sctx.Config.CI = ci

	result := scopeRun{agent: ag}
	sctx.Log = func(s string) { result.logs = append(result.logs, s) }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx
	polls := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, _ time.Duration) error {
		polls++
		if polls >= maxPolls {
			cancel()
			return ctx.Err()
		}
		return nil
	})
	result.outcome, result.err = step.Execute(sctx)
	result.ghLog = ghLog(t, logFile)
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	result.ciReady = run.CIReadyAt != nil
	return result
}

const (
	buildPass       = `{"name":"build","state":"SUCCESS","bucket":"pass"}`
	buildFail       = `{"name":"build","state":"FAILURE","bucket":"fail"}`
	approveFail     = `{"name":"approve","state":"FAILURE","bucket":"fail","workflow":"Final Approval"}`
	approvalStatus  = `{"name":"approval-status","state":"FAILURE","bucket":"fail"}`
	protectBuild    = `FAKE_CLI_PROTECTION_JSON={"strict":false,"contexts":["build"],"checks":[{"context":"build","app_id":-1}]}`
	protectBuildApp = `FAKE_CLI_PROTECTION_JSON={"contexts":["build","approve"],"checks":[{"context":"build"},{"context":"approve"}]}`
)

func rollup(checks ...string) []string { return []string{"[" + strings.Join(checks, ",") + "]"} }

func assertReady(t *testing.T, r scopeRun) {
	t.Helper()
	if !errors.Is(r.err, context.Canceled) || r.outcome != nil {
		t.Fatalf("Execute() = %+v, %v; want monitoring to continue past green. logs: %v", r.outcome, r.err, r.logs)
	}
	if !r.reportedPassed() || !r.ciReady {
		t.Fatalf("passed logged = %v, ci_ready = %v; logs: %v", r.reportedPassed(), r.ciReady, r.logs)
	}
	if len(r.agent.Calls) != 0 {
		t.Fatalf("fix agent was invoked %d times", len(r.agent.Calls))
	}
}

func assertBlockedOn(t *testing.T, r scopeRun, blocking string, notBlocking ...string) {
	t.Helper()
	if r.err != nil || r.outcome == nil || !r.outcome.NeedsApproval {
		t.Fatalf("Execute() = %+v, %v; want a failing-CI finding for %q. logs: %v", r.outcome, r.err, blocking, r.logs)
	}
	if !strings.Contains(r.outcome.Findings, `"check":"`+blocking+`"`) {
		t.Fatalf("findings %s do not name %q", r.outcome.Findings, blocking)
	}
	for _, name := range notBlocking {
		if strings.Contains(r.outcome.Findings, `"check":"`+name+`"`) {
			t.Fatalf("findings %s must not name %q", r.outcome.Findings, name)
		}
	}
	if r.reportedPassed() || r.ciReady {
		t.Fatalf("a blocked head was reported ready: logs %v", r.logs)
	}
}

func TestCIStep_FailingNonRequiredCheckDoesNotBlock(t *testing.T) {
	t.Parallel()
	r := runScoped(t, rollup(buildPass, approveFail, approvalStatus), []string{protectBuild}, config.CI{}, 1)
	assertReady(t, r)
	if !r.logged("informational (not required, ignored): approval-status (fail), approve (fail)") {
		t.Fatalf("the ignored checks were not reported; logs: %v", r.logs)
	}
	if !r.logged(`"main" requires 1 status check(s): build`) {
		t.Fatalf("the required set was not reported; logs: %v", r.logs)
	}
}

func TestCIStep_FailingRequiredCheckStillBlocksAndIsRepaired(t *testing.T) {
	t.Parallel()
	r := runScoped(t, rollup(buildFail, approveFail), []string{protectBuild}, config.CI{}, 1)
	assertBlockedOn(t, r, "build", "approve")
	if !r.outcome.AutoFixable {
		t.Fatalf("a failing required check must stay auto-fixable: %+v", r.outcome)
	}
}

func TestCIStep_ExternalChecksBeatTheRequiredSet(t *testing.T) {
	t.Parallel()
	ci := config.CI{ExternalChecks: []string{"Final Approval / approve*", "approval-status"}}
	// Both approval contexts are required by the branch, and still never block.
	env := []string{`FAKE_CLI_PROTECTION_JSON={"contexts":["build","approve","approval-status"]}`}
	r := runScoped(t, rollup(buildPass, approveFail, approvalStatus), env, ci, 1)
	assertReady(t, r)
	if !r.logged("external (awaiting outside approval): approval-status (fail), approve (fail)") {
		t.Fatalf("external checks were not reported as such; logs: %v", r.logs)
	}
	if r.logged("informational") {
		t.Fatalf("external checks were reported as informational; logs: %v", r.logs)
	}
}

func TestCIStep_ExternalChecksApplyWithoutBranchProtection(t *testing.T) {
	t.Parallel()
	ci := config.CI{ExternalChecks: []string{"approve"}}
	r := runScoped(t, rollup(buildPass, approveFail), nil, ci, 1)
	assertReady(t, r)
	r = runScoped(t, rollup(buildFail, approveFail), nil, ci, 1)
	assertBlockedOn(t, r, "build", "approve")
}

func TestCIStep_MustPassAddsARequiredCheck(t *testing.T) {
	t.Parallel()
	lintFail := `{"name":"lint (go)","state":"FAILURE","bucket":"fail"}`
	ci := config.CI{MustPass: []string{"lint*"}}
	r := runScoped(t, rollup(buildPass, lintFail, approveFail), []string{protectBuild}, ci, 1)
	assertBlockedOn(t, r, "lint (go)", "approve")
	r = runScoped(t, rollup(buildPass, lintFail, approveFail), []string{protectBuild}, config.CI{}, 1)
	assertReady(t, r)
}

func TestCIStep_UnreadableProtectionFallsBackToAllChecks(t *testing.T) {
	t.Parallel()
	for name, env := range map[string][]string{
		"no protection": nil,
		"forbidden":     {"FAKE_CLI_PROTECTION_JSON=403", "FAKE_CLI_RULES_JSON=403"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := runScoped(t, rollup(buildPass, approveFail), env, config.CI{}, 1)
			assertBlockedOn(t, r, "approve")
			if !r.logged("evaluating all checks") {
				t.Fatalf("the fallback was not logged; logs: %v", r.logs)
			}
		})
	}
}

func TestCIStep_RequiredChecksAllIgnoresBranchProtection(t *testing.T) {
	t.Parallel()
	r := runScoped(t, rollup(buildPass, approveFail), []string{protectBuild}, config.CI{RequiredChecks: "all"}, 1)
	assertBlockedOn(t, r, "approve")
	if got := r.ghCalls("required_status_checks"); got != 0 {
		t.Fatalf("ci.required_checks: all still read branch protection %d times", got)
	}
}

func TestCIStep_RulesetRequiredChecksAreUnionedWithProtection(t *testing.T) {
	t.Parallel()
	rules := `FAKE_CLI_RULES_JSON=[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"test"}]}},{"type":"deletion"}]`
	testFail := `{"name":"test","state":"FAILURE","bucket":"fail"}`
	// Only a ruleset requires "test": protection alone would have ignored it.
	r := runScoped(t, rollup(buildPass, testFail, approveFail), []string{protectBuild, rules}, config.CI{}, 1)
	assertBlockedOn(t, r, "test", "approve")
	// Rulesets alone also define the required set.
	testPass := `{"name":"test","state":"SUCCESS","bucket":"pass"}`
	r = runScoped(t, rollup(testPass, approveFail), []string{rules}, config.CI{}, 1)
	assertReady(t, r)
}

func TestCIStep_MissingRequiredCheckBlocksReadiness(t *testing.T) {
	t.Parallel()
	env := []string{`FAKE_CLI_PROTECTION_JSON={"contexts":["build","deploy"]}`}
	r := runScoped(t, rollup(buildPass), env, config.CI{}, 2)
	if !errors.Is(r.err, context.Canceled) || r.outcome != nil {
		t.Fatalf("Execute() = %+v, %v; want monitoring to keep waiting", r.outcome, r.err)
	}
	if r.reportedPassed() || r.ciReady {
		t.Fatalf("a head missing the required deploy check was reported ready; logs: %v", r.logs)
	}
	r = runScoped(t, []string{"[" + buildPass + "]", "[" + buildPass + `,{"name":"deploy","state":"SUCCESS","bucket":"pass"}]`}, env, config.CI{}, 2)
	assertReady(t, r)
}

func TestCIStep_RequiredChecksAreReadOncePerExecution(t *testing.T) {
	t.Parallel()
	pending := `{"name":"build","state":"PENDING","bucket":"pending"}`
	checks := []string{"[" + pending + "]", "[" + pending + "]", "[" + pending + "]", "[" + buildPass + "]"}
	r := runScoped(t, checks, []string{protectBuild}, config.CI{}, 4)
	if !r.reportedPassed() {
		t.Fatalf("the sequence never settled; logs: %v", r.logs)
	}
	if got := strings.Count(r.ghLog, "graphql"); got < 4 {
		t.Fatalf("expected a check read per poll, saw %d graphql reads", got)
	}
	if protection, rules := r.ghCalls("/protection/required_status_checks"), r.ghCalls("/rules/branches/"); protection != 1 || rules != 1 {
		t.Fatalf("protection reads = %d, rules reads = %d across four polls, want one of each:\n%s", protection, rules, r.ghLog)
	}
}

func TestCIStep_RequiredCheckBoundToAnAppIgnoresSameNamedChecksFromOthers(t *testing.T) {
	t.Parallel()
	env := []string{`FAKE_CLI_PROTECTION_JSON={"contexts":["build"],"checks":[{"context":"build","app_id":15368}]}`}
	otherApp := `{"name":"build","state":"SUCCESS","bucket":"pass","app":"some-other-app","appId":99}`
	r := runScoped(t, rollup(otherApp), env, config.CI{}, 2)
	if !errors.Is(r.err, context.Canceled) || r.outcome != nil || r.reportedPassed() || r.ciReady {
		t.Fatalf("a same-named check from another app satisfied the bound requirement: outcome=%+v err=%v logs=%v", r.outcome, r.err, r.logs)
	}
	owner := `{"name":"build","state":"SUCCESS","bucket":"pass","app":"github-actions","appId":15368}`
	r = runScoped(t, rollup(owner), env, config.CI{}, 1)
	assertReady(t, r)
}

func TestCIStep_MissingRequiredCheckDoesNotDelayBuildRepair(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	ag := &stepstest.MockAgent{AgentName: "test", RunFn: func(context.Context, agent.RunOpts) (*agent.Result, error) {
		return nil, errors.New("stop after repair dispatch")
	}}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(stepstest.FakeCIGHSequence(t, "OPEN", rollup(buildFail)),
		`FAKE_CLI_PROTECTION_JSON={"contexts":["build","lint"]}`, "FAKE_CLI_RULES_JSON=[]")
	stepstest.BindCurrentPRComparison(t, sctx, "https://github.com/test/repo/pull/42", "test/repo", "main")
	sctx.Config.CITimeout = 10 * time.Minute
	sctx.Config.AutoFix.CI = 1
	waits := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(context.Context, time.Duration) error {
		waits++
		return context.Canceled
	})
	_, _ = stepstest.ExecuteWithAutoFix(t, step, sctx, 0)
	if len(ag.Calls) != 1 || waits != 0 {
		t.Fatalf("repair calls = %d, poll waits = %d; want immediate repair dispatch", len(ag.Calls), waits)
	}
	if !strings.Contains(sctx.PreviousFindings, `"check":"build"`) || strings.Contains(sctx.PreviousFindings, `"check":"lint"`) {
		t.Fatalf("repair selection = %s, want only build", sctx.PreviousFindings)
	}
	run, err := sctx.DB.GetRun(sctx.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run.CIReadyAt != nil {
		t.Fatal("missing lint must keep readiness blocked")
	}
}

func TestCIStep_RequiredPipelineStatusDoesNotBlockReadiness(t *testing.T) {
	t.Parallel()
	env := []string{
		`FAKE_CLI_PROTECTION_JSON={"contexts":["build","no-mistakes"],"checks":[{"context":"build"},{"context":"no-mistakes"}]}`,
		`FAKE_CLI_RULES_JSON=[{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"no-mistakes"}]}}]`,
	}
	r := runScoped(t, rollup(buildPass), env, config.CI{}, 1)
	assertReady(t, r)
}

func TestCIStep_TargetReadFailuresHonorTimeout(t *testing.T) {
	for _, expireDuringRead := range []bool{false, true} {
		t.Run(map[bool]string{false: "repeated errors", true: "deadline expires during read"}[expireDuringRead], func(t *testing.T) {
			dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
			env, logFile := stepstest.FakeCIGHLoggedSequence(t, "OPEN", []string{`[{"name":"build","state":"SUCCESS","bucket":"pass"}]`}, "", "")
			targetFile := filepath.Join(t.TempDir(), "target")
			if err := os.WriteFile(targetFile, []byte("main"), 0600); err != nil {
				t.Fatal(err)
			}
			sctx := stepstest.NewTestContextWithDBRecords(t, nil, dir, baseSHA, headSHA, config.Commands{})
			sctx.Env = append(env, "FAKE_CLI_PR_BASE_FILE="+targetFile)
			stepstest.BindCurrentPRComparison(t, sctx, "https://github.com/test/repo/pull/42", "test/repo", "main")
			sctx.Config.CITimeout = 10 * time.Second
			started := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			current := started
			failures, waits := 0, 0
			sctx.Log = func(message string) {
				if strings.Contains(message, "could not read the PR target branch") {
					failures++
					if expireDuringRead {
						current = started.Add(sctx.Config.CITimeout)
					}
				}
			}
			step := (&steps.CIStep{}).SetNow(func() time.Time { return current }).SetBaseBranchTip(func(context.Context) (string, bool) { return baseSHA, true })
			step.SetWaitForNextPoll(func(_ context.Context, interval time.Duration) error {
				waits++
				if interval <= 0 {
					t.Fatalf("nonpositive poll wait: %v", interval)
				}
				if waits > 4 {
					return errors.New("target read retry exceeded deadline")
				}
				if err := os.WriteFile(targetFile, []byte("ERROR"), 0600); err != nil {
					t.Fatal(err)
				}
				current = current.Add(4 * time.Second)
				return nil
			})
			outcome, err := step.Execute(sctx)
			if err != nil || outcome == nil || !outcome.NeedsApproval {
				t.Fatalf("expected timeout approval gate: %+v, %v", outcome, err)
			}
			if failures == 0 || failures > 2 || waits > 3 {
				t.Fatalf("target retries not bounded: failures=%d waits=%d calls=%s", failures, waits, ghLog(t, logFile))
			}
			run, err := sctx.DB.GetRun(sctx.Run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if run.CIReadyAt != nil {
				t.Fatal("unreadable target retained readiness")
			}
		})
	}
}

// Timeout evidence judged the old target. A retarget observed as the idle
// deadline passes must not report it before a poll under the new scope replaces it.
func TestCIStep_RetargetAtDeadlineDropsTheOldTargetsTimeoutEvidence(t *testing.T) {
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)
	env, _ := stepstest.FakeCIGHLoggedSequence(t, "OPEN", []string{`[{"name":"old-only","state":"FAILURE","bucket":"fail"},{"name":"new-req","state":"PENDING","bucket":"pending"}]`}, "", "")
	targetFile := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(targetFile, []byte("main"), 0600); err != nil {
		t.Fatal(err)
	}
	sctx := stepstest.NewTestContextWithDBRecords(t, nil, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_PR_BASE_FILE="+targetFile,
		`FAKE_CLI_PROTECTION_JSON_MAIN={"contexts":["old-only","new-req"]}`,
		`FAKE_CLI_PROTECTION_JSON_RELEASE={"contexts":["new-req"]}`,
		`FAKE_CLI_RULES_JSON=[]`)
	stepstest.BindCurrentPRComparison(t, sctx, "https://github.com/test/repo/pull/42", "test/repo", "main")
	sctx.Config.CITimeout = 10 * time.Second
	current := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	step := (&steps.CIStep{}).SetNow(func() time.Time { return current }).SetBaseBranchTip(func(context.Context) (string, bool) { return baseSHA, true })
	started := current
	retargeted := false
	sctx.Log = func(message string) {
		if strings.Contains(message, "PR target changed from main to release") {
			retargeted = true
			current = started.Add(sctx.Config.CITimeout)
		}
	}
	step.SetWaitForNextPoll(func(context.Context, time.Duration) error {
		current = current.Add(time.Second)
		return os.WriteFile(targetFile, []byte("release"), 0600)
	})
	outcome, err := step.Execute(sctx)
	if err != nil || outcome == nil || !outcome.NeedsApproval || !retargeted {
		t.Fatalf("expected the timeout gate after the retarget: %+v, %v, retargeted=%v", outcome, err, retargeted)
	}
	if strings.Contains(outcome.Findings, "old-only") {
		t.Fatalf("the old target's failure was reported after the retarget: %s", outcome.Findings)
	}
}
