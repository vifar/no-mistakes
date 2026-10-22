package citest

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/cimonitor"
	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps"
	"github.com/kunchenguid/no-mistakes/internal/pipeline/steps/internal/stepstest"
)

// Issue #158 is about the TEST gate, not the CI gate. The no_ci contract
// stays exactly as it is: "when true and the forge reports ZERO checks on the
// PR head" the run treats the head as all-checks-passed, and the declaration
// never waives a pending or failing check that is in scope. These tests pin
// that contract so the accepted-gap change cannot quietly widen it.

// A declared no-CI repository with zero observed checks still reads as
// all-checks-passed.
func TestNoCIWithZeroChecksStillReadsAsAllChecksPassed(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	env := stepstest.FakeCIGH(t, "OPEN", "[]")

	prURL := "https://github.com/test/repo/pull/42"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = env
	sctx.Run.PRURL = &prURL
	stepstest.BindCurrentPRComparison(t, sctx, prURL, "test/repo", "main")
	sctx.Config.CITimeout = 10 * time.Second
	sctx.Config.NoCI = true

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		cancel()
		return ctx.Err()
	})
	if _, err := step.Execute(sctx); err == nil {
		t.Fatal("expected the CI monitor to keep watching after the no-CI ready marker")
	}
	found := false
	for _, l := range logs {
		if l == cimonitor.NoChecksPassedMsg {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected the declared no_ci ready marker, got: %v", logs)
	}
	if !cimonitor.ChecksPassed(logs) {
		t.Fatalf("declared no_ci with zero checks must read as all checks passed, logs: %v", logs)
	}
	if !cimonitor.DeclaredNoCI(logs) {
		t.Fatal("the declared no_ci ready path must stay inspectable")
	}
}

// The declaration never waives a pending or failing in-scope check.
func TestNoCINeverWaivesAPendingOrFailingCheck(t *testing.T) {
	t.Parallel()
	dir, baseSHA, headSHA := stepstest.SetupGitRepo(t)

	checksSequence := []string{
		`[{"name":"surprise","state":"PENDING","bucket":"pending"}]`,
		`[{"name":"surprise","state":"FAILURE","bucket":"fail","completedAt":"2026-07-30T08:06:01Z"}]`,
	}
	env := stepstest.FakeCIGHSequence(t, "OPEN", checksSequence)

	prURL := "https://github.com/test/repo/pull/99"
	ag := &stepstest.MockAgent{AgentName: "test"}
	sctx := stepstest.NewTestContextWithDBRecords(t, ag, dir, baseSHA, headSHA, config.Commands{})
	sctx.Env = append(env, "FAKE_CLI_PR_URL="+prURL)
	sctx.Run.PRURL = &prURL
	stepstest.BindCurrentPRComparison(t, sctx, prURL, "test/repo", "main")
	sctx.Config.CITimeout = 10 * time.Minute
	sctx.Config.NoCI = true
	zero := 0
	sctx.Config.AutoFix.CI = zero

	var logs []string
	sctx.Log = func(s string) { logs = append(logs, s) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sctx.Ctx = ctx

	phase := 0
	step := (&steps.CIStep{}).SetWaitForNextPoll(func(ctx context.Context, interval time.Duration) error {
		phase++
		if phase == 1 && cimonitor.ChecksPassed(logs) {
			t.Fatalf("a pending in-scope check must not be waived by no_ci; logs=%v", logs)
		}
		return nil
	})
	outcome, err := step.Execute(sctx)
	if err != nil {
		t.Fatalf("expected a failure outcome, got err=%v", err)
	}
	if outcome == nil || !outcome.NeedsApproval {
		t.Fatalf("expected the failing in-scope check to park, got %#v", outcome)
	}
	for _, l := range logs {
		if l == cimonitor.NoChecksPassedMsg {
			t.Fatalf("the no_ci ready marker must not fire while a check is registered; logs=%v", logs)
		}
	}
	if cimonitor.ChecksPassed(logs) {
		t.Fatalf("a failing in-scope check must not be waived by no_ci; logs=%v", logs)
	}
}
