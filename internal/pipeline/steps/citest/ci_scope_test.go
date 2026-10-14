package citest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
