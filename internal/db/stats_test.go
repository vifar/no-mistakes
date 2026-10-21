package db

import (
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestStepFindingStatsIndependentWithdrawalIsNotACodeFix(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/fixture", "git@example.com:owner/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	payloads := []string{
		`{"findings":[{"id":"review-1","severity":"error","description":"outside scope","action":"ask-user"}]}`,
		`{"findings":[{"id":"review-1","severity":"error","description":"new defect reusing the id","action":"auto-fix"}],"withdrawn_findings":[{"id":"review-1","reason":"independent review confirms accepted scope"}]}`,
		`{"findings":[]}`,
	}
	for i, raw := range payloads {
		if _, err := d.InsertStepRound(step.ID, i+1, "auto_fix", &raw, nil, 1); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := d.StepFindingStats(step)
	if err != nil || stats.ReportedFindings != 2 || stats.FixedFindings != 1 {
		t.Fatalf("scope withdrawal must not count as fixing either reused id: %+v, %v", stats, err)
	}
}

func TestGetStatsAggregatesReportedFixesAndRescueRuns(t *testing.T) {
	d := openTestDB(t)
	repoA, _ := d.InsertRepo("/repo/a", "git@example.com:a.git", "main")
	repoB, _ := d.InsertRepo("/repo/b", "git@example.com:b.git", "main")

	runA, _ := d.InsertRun(repoA.ID, "feature-a", "head-a", "base-a")
	reviewA, _ := d.InsertStepResult(runA.ID, types.StepReview)
	reviewAInitial := `{"findings":[{"id":"r1","severity":"warning","description":"one","action":"auto-fix"},{"id":"r2","severity":"warning","description":"two","action":"auto-fix"},{"id":"r3","severity":"warning","description":"three","action":"auto-fix"}],"summary":"three","risk_level":"medium","risk_rationale":"test"}`
	reviewAFinal := `{"findings":[{"id":"r3","severity":"warning","description":"three","action":"ask-user"}],"summary":"one left","risk_level":"medium","risk_rationale":"test"}`
	if _, err := d.InsertStepRound(reviewA.ID, 1, "initial", &reviewAInitial, nil, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(reviewA.ID, 2, "auto_fix", &reviewAFinal, nil, 100); err != nil {
		t.Fatal(err)
	}

	lintA, _ := d.InsertStepResult(runA.ID, types.StepLint)
	lintAInitial := `{"findings":[{"id":"l1","severity":"error","description":"lint","action":"auto-fix"}],"summary":"one","risk_level":"low","risk_rationale":"test"}`
	if _, err := d.InsertStepRound(lintA.ID, 1, "initial", &lintAInitial, nil, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(lintA.ID, 2, "auto_fix", nil, nil, 100); err != nil {
		t.Fatal(err)
	}

	runB, _ := d.InsertRun(repoB.ID, "feature-b", "head-b", "base-b")
	testB, _ := d.InsertStepResult(runB.ID, types.StepTest)
	testBInitial := `{"findings":[{"id":"t1","severity":"error","description":"test","action":"ask-user"}],"summary":"one","risk_level":"low","risk_rationale":"test"}`
	if _, err := d.InsertStepRound(testB.ID, 1, "initial", &testBInitial, nil, 100); err != nil {
		t.Fatal(err)
	}

	stats, err := d.GetStats()
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}

	if stats.TotalRuns != 2 {
		t.Fatalf("TotalRuns = %d, want 2", stats.TotalRuns)
	}
	if stats.RescueRuns != 1 {
		t.Fatalf("RescueRuns = %d, want 1", stats.RescueRuns)
	}
	if stats.ReportedFindings != 5 {
		t.Fatalf("ReportedFindings = %d, want 5", stats.ReportedFindings)
	}
	if stats.FixedFindings != 3 {
		t.Fatalf("FixedFindings = %d, want 3", stats.FixedFindings)
	}

	assertStepStat(t, stats.StepStats, types.StepReview, 3, 2)
	assertStepStat(t, stats.StepStats, types.StepLint, 1, 1)
	assertStepStat(t, stats.StepStats, types.StepTest, 1, 0)

	if len(stats.RepoStats) != 2 {
		t.Fatalf("len(RepoStats) = %d, want 2", len(stats.RepoStats))
	}
	if stats.RepoStats[0].WorkingPath != "/repo/a" {
		t.Fatalf("top repo = %q, want /repo/a", stats.RepoStats[0].WorkingPath)
	}
	if stats.RepoStats[0].RescueRuns != 1 || stats.RepoStats[0].FixedFindings != 3 {
		t.Fatalf("top repo stats = %#v, want rescue 1 fixes 3", stats.RepoStats[0])
	}
}

func TestGetStatsFallsBackToStepFindingsWhenRoundsAreMissing(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/repo/legacy", "git@example.com:legacy.git", "main")
	run, _ := d.InsertRun(repo.ID, "legacy", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	findings := `{"findings":[{"id":"legacy-1","severity":"warning","description":"legacy","action":"ask-user"}],"summary":"one","risk_level":"low","risk_rationale":"test"}`
	if err := d.SetStepFindings(step.ID, findings); err != nil {
		t.Fatal(err)
	}

	stats, err := d.GetStats()
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}

	if stats.ReportedFindings != 1 {
		t.Fatalf("ReportedFindings = %d, want 1", stats.ReportedFindings)
	}
	if stats.FixedFindings != 0 {
		t.Fatalf("FixedFindings = %d, want 0", stats.FixedFindings)
	}
	if stats.RescueRuns != 0 {
		t.Fatalf("RescueRuns = %d, want 0", stats.RescueRuns)
	}
}

func TestFixedFindingsByStepCountsResolvedRoundFindings(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/repo/fixes", "git@example.com:fixes.git", "main")
	run, _ := d.InsertRun(repo.ID, "fixes", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	initial := `{"findings":[{"id":"r1","severity":"warning","description":"one"},{"id":"r2","severity":"warning","description":"two"},{"id":"r3","severity":"warning","description":"three"}],"summary":"three"}`
	final := `{"findings":[{"id":"r3","severity":"warning","description":"three"}],"summary":"one left"}`
	if _, err := d.InsertStepRound(step.ID, 1, "initial", &initial, nil, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(step.ID, 2, "auto_fix", &final, nil, 100); err != nil {
		t.Fatal(err)
	}

	fixed, err := d.FixedFindingsByStep(step)
	if err != nil {
		t.Fatalf("fixed findings by step: %v", err)
	}
	if fixed != 2 {
		t.Fatalf("fixed = %d, want 2", fixed)
	}
}

func TestStepFindingStatsDoesNotCountSelectedFindingsAsFixed(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/repo/fixing", "git@example.com:fixing.git", "main")
	run, _ := d.InsertRun(repo.ID, "fixing", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	initial := `{"findings":[{"id":"r1","severity":"warning","description":"one"},{"id":"r2","severity":"warning","description":"two"},{"id":"r3","severity":"warning","description":"three"}],"summary":"three"}`
	round, err := d.InsertStepRound(step.ID, 1, "initial", &initial, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	selected := `["r1","r2"]`
	if err := d.SetStepRoundSelection(round.ID, &selected, RoundSelectionSourceUser); err != nil {
		t.Fatal(err)
	}

	stats, err := d.StepFindingStats(step)
	if err != nil {
		t.Fatalf("step finding stats: %v", err)
	}
	if stats.ReportedFindings != 3 || stats.FixedFindings != 0 {
		t.Fatalf("stats = reported %d fixed %d, want reported 3 fixed 0", stats.ReportedFindings, stats.FixedFindings)
	}
}

func TestStepFindingStatsAddsNewFindingsToTotal(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/repo/new-findings", "git@example.com:new.git", "main")
	run, _ := d.InsertRun(repo.ID, "new", "head", "base")
	step, _ := d.InsertStepResult(run.ID, types.StepReview)
	initial := `{"findings":[{"id":"r1","severity":"warning","description":"one"},{"id":"r2","severity":"warning","description":"two"},{"id":"r3","severity":"warning","description":"three"}],"summary":"three"}`
	final := `{"findings":[{"id":"r3","severity":"warning","description":"three"},{"id":"r4","severity":"warning","description":"four"}],"summary":"two left"}`
	if _, err := d.InsertStepRound(step.ID, 1, "initial", &initial, nil, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(step.ID, 2, "auto_fix", &final, nil, 100); err != nil {
		t.Fatal(err)
	}

	stats, err := d.StepFindingStats(step)
	if err != nil {
		t.Fatalf("step finding stats: %v", err)
	}
	if stats.ReportedFindings != 4 || stats.FixedFindings != 2 {
		t.Fatalf("stats = reported %d fixed %d, want reported 4 fixed 2", stats.ReportedFindings, stats.FixedFindings)
	}
}

func assertStepStat(t *testing.T, stats []StepStats, step types.StepName, reported int, fixes int) {
	t.Helper()
	for _, got := range stats {
		if got.StepName != step {
			continue
		}
		if got.ReportedFindings != reported || got.FixedFindings != fixes {
			t.Fatalf("step %s = reported %d fixes %d, want reported %d fixes %d", step, got.ReportedFindings, got.FixedFindings, reported, fixes)
		}
		return
	}
	t.Fatalf("missing stats for step %s", step)
}

func TestGetStatsDoesNotCountTestBudgetCutsAsFixedMistakes(t *testing.T) {
	d := openTestDB(t)
	repo, _ := d.InsertRepo("/repo/cut", "git@example.com:cut.git", "main")

	// Two cuts in a row, then a validation-only fix round that completes. Each
	// cut's description names a different killed process.
	cutOnly, _ := d.InsertRun(repo.ID, "cut-only", "head", "base")
	cutOnlyTest, _ := d.InsertStepResult(cutOnly.ID, types.StepTest)
	firstCut := `{"findings":[{"id":"test-agent-timeout","severity":"warning","description":"cut (pid 101)","action":"ask-user"},{"id":"test-agent-unvalidated-work","severity":"error","description":"holds foo_test.go","action":"ask-user"}],"summary":"cut"}`
	secondCut := `{"findings":[{"id":"test-agent-timeout","severity":"warning","description":"cut (pid 202)","action":"ask-user"}],"summary":"cut"}`
	if _, err := d.InsertStepRound(cutOnlyTest.ID, 1, "initial", &firstCut, nil, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(cutOnlyTest.ID, 2, "user_fix", &secondCut, nil, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(cutOnlyTest.ID, 3, "user_fix", nil, nil, 100); err != nil {
		t.Fatal(err)
	}

	// A real defect parked beside a cut still counts once it is fixed.
	mixed, _ := d.InsertRun(repo.ID, "mixed", "head", "base")
	mixedTest, _ := d.InsertStepResult(mixed.ID, types.StepTest)
	mixedCut := `{"findings":[{"id":"test-agent-timeout","severity":"warning","description":"cut (pid 303)","action":"ask-user"},{"id":"test-1","severity":"error","description":"TestFoo fails","action":"auto-fix"}],"summary":"cut"}`
	if _, err := d.InsertStepRound(mixedTest.ID, 1, "initial", &mixedCut, nil, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(mixedTest.ID, 2, "user_fix", nil, nil, 100); err != nil {
		t.Fatal(err)
	}

	stats, err := d.GetStats()
	if err != nil {
		t.Fatalf("get stats: %v", err)
	}
	if stats.ReportedFindings != 1 || stats.FixedFindings != 1 || stats.RescueRuns != 1 {
		t.Fatalf("stats = reported %d fixed %d rescued %d, want reported 1 fixed 1 rescued 1 (only the real defect)", stats.ReportedFindings, stats.FixedFindings, stats.RescueRuns)
	}
	assertStepStat(t, stats.StepStats, types.StepTest, 1, 1)

	cutOnlyStats, err := d.StepFindingStats(cutOnlyTest)
	if err != nil {
		t.Fatal(err)
	}
	if cutOnlyStats.ReportedFindings != 0 || cutOnlyStats.FixedFindings != 0 {
		t.Fatalf("cut-only step = reported %d fixed %d, want 0/0", cutOnlyStats.ReportedFindings, cutOnlyStats.FixedFindings)
	}
}

func TestCarriedFindingStatisticsCountExecutedWorkOnce(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/fixture/carried-stats", "git@example.com:carried.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	source, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	review, err := d.InsertStepResult(source.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	const pending = `{"findings":[{"id":"claim-test","severity":"warning","action":"no-op","category":"review-support-pending","description":"needs Test proof","support":{"claim_type":"test","test":{"command":"go test ./internal/types"}}},{"id":"claim-ci","severity":"warning","action":"no-op","category":"review-support-pending","description":"needs CI proof","support":{"claim_type":"ci","ci":{"check_id":"42","head_sha":"head"}}}]}`
	const initial = `{"findings":[{"id":"fixed","severity":"error","action":"auto-fix","description":"corrected defect"},{"id":"claim-test","severity":"warning","action":"no-op","category":"review-support-pending","description":"needs Test proof","support":{"claim_type":"test","test":{"command":"go test ./internal/types"}}},{"id":"claim-ci","severity":"warning","action":"no-op","category":"review-support-pending","description":"needs CI proof","support":{"claim_type":"ci","ci":{"check_id":"42","head_sha":"head"}}}]}`
	for i, raw := range []string{initial, pending} {
		if _, err := d.InsertStepRound(review.ID, i+1, []string{"initial", "auto_fix"}[i], &raw, nil, 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.SetStepFindings(review.ID, pending); err != nil {
		t.Fatal(err)
	}
	if err := d.CompleteStepWithStatus(review.ID, types.StepStatusCompleted, 0, 20, ""); err != nil {
		t.Fatal(err)
	}
	review, err = d.GetStepResult(review.ID)
	if err != nil {
		t.Fatal(err)
	}
	origin := review.ID
	reviews := []*StepResult{review}
	previous := source
	for range 2 {
		test, err := d.InsertStepResult(previous.ID, types.StepTest)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.FailStepWithCause(test.ID, "agent invocation failed", 1, StepFailureCauseAgentInvocation); err != nil {
			t.Fatal(err)
		}
		if err := d.UpdateRunStatus(previous.ID, types.RunFailed); err != nil {
			t.Fatal(err)
		}
		next, err := d.InsertRun(repo.ID, "feature", "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		if err := d.InsertRunRetry(RunRetry{RunID: next.ID, SourceRunID: previous.ID, StepName: types.StepTest, SourceRound: 1, SourceError: "agent invocation failed", CheckpointHeadSHA: "head"}); err != nil {
			t.Fatal(err)
		}
		carried, err := d.InsertCarriedStepResult(next.ID, reviews[len(reviews)-1])
		if err != nil {
			t.Fatal(err)
		}
		carried, err = d.GetStepResult(carried.ID)
		if err != nil {
			t.Fatal(err)
		}
		reviews = append(reviews, carried)
		previous = next
	}
	otherRepo, err := d.InsertRepo("/fixture/executed-stats", "git@example.com:executed.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	executed, err := d.InsertRun(otherRepo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	test, err := d.InsertStepResult(executed.ID, types.StepTest)
	if err != nil {
		t.Fatal(err)
	}
	for i, raw := range []string{`{"findings":[{"id":"test-fixed","severity":"error","description":"executed test defect"}]}`, `{"findings":[]}`} {
		if _, err := d.InsertStepRound(test.ID, i+1, []string{"initial", "auto_fix"}[i], &raw, nil, 10); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.CompleteStepWithStatus(test.ID, types.StepStatusCompleted, 0, 20, ""); err != nil {
		t.Fatal(err)
	}
	if err := d.UpdateRunStatus(executed.ID, types.RunCompleted); err != nil {
		t.Fatal(err)
	}

	for _, row := range reviews {
		if row.FindingsJSON == nil || *row.FindingsJSON != pending || row.RoundsStepID() != origin {
			t.Fatalf("enforcement payload or lineage changed: %+v", row)
		}
		got, err := d.StepFindingStats(row)
		if err != nil || got.StepName != types.StepReview || got.ReportedFindings != 3 || got.FixedFindings != 1 {
			t.Fatalf("individual step lost source statistics: %+v err=%v", got, err)
		}
		fixed, err := d.FixedFindingsByStep(row)
		if err != nil || fixed != 1 {
			t.Fatalf("fixed findings lost source lineage: fixed=%d err=%v", fixed, err)
		}
		rounds, err := d.GetRoundsByStep(row.RoundsStepID())
		if err != nil || len(rounds) != 2 {
			t.Fatalf("source rounds unavailable: rounds=%+v err=%v", rounds, err)
		}
		for _, round := range rounds {
			if round.StepResultID != origin {
				t.Fatalf("round source identity rewritten: %+v", round)
			}
		}
		if row.ID != origin {
			ownRounds, err := d.GetRoundsByStep(row.ID)
			if err != nil || len(ownRounds) != 0 {
				t.Fatalf("carry acquired duplicated execution rounds: rounds=%+v err=%v", ownRounds, err)
			}
			totals := map[types.StepName]*StepStats{}
			reported, fixed, err := d.aggregateRunStats(row.RunID, totals)
			if err != nil || reported != 0 || fixed != 0 || totals[types.StepReview] != nil {
				t.Fatalf("carry counted as executed work: reported=%d fixed=%d steps=%+v err=%v", reported, fixed, totals, err)
			}
		}
	}
	stats, err := d.GetStats()
	if err != nil {
		t.Fatal(err)
	}
	if stats.TotalRepos != 2 || stats.TotalRuns != 4 || stats.ReportedFindings != 4 || stats.FixedFindings != 2 || stats.RescueRuns != 2 {
		t.Fatalf("historical totals inflated or executed work lost: %+v", stats)
	}
	assertStepStat(t, stats.StepStats, types.StepReview, 3, 1)
	assertStepStat(t, stats.StepStats, types.StepTest, 1, 1)
	if len(stats.RepoStats) != 2 {
		t.Fatalf("unexpected repositories: %+v", stats.RepoStats)
	}
	for _, repoStats := range stats.RepoStats {
		if repoStats.RepoID == repo.ID {
			if repoStats.Runs != 3 || repoStats.ReportedFindings != 3 || repoStats.FixedFindings != 1 || repoStats.RescueRuns != 1 {
				t.Fatalf("carried repository totals inflated: %+v", repoStats)
			}
		} else if repoStats.RepoID == otherRepo.ID {
			if repoStats.Runs != 1 || repoStats.ReportedFindings != 1 || repoStats.FixedFindings != 1 || repoStats.RescueRuns != 1 {
				t.Fatalf("executed repository totals suppressed: %+v", repoStats)
			}
		} else {
			t.Fatalf("unexpected repository: %+v", repoStats)
		}
	}
}

func TestStepFindingStatsDistinguishesSupportClaimsByValue(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo("/fixture/support-counts", "git@example.com:support.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	initial := `{"findings":[{"id":"a","severity":"warning","description":"needs proof","support":{"claim_type":"test","test":{"command":"go test ./a"}}},{"id":"b","severity":"warning","description":"needs proof","support":{"claim_type":"test","test":{"command":"go test ./b"}}}]}`
	final := `{"findings":[{"id":"b","severity":"warning","description":"needs proof","support":{"claim_type":"test","test":{"command":"go test ./b"}}}]}`
	for i, raw := range []string{initial, final} {
		if _, err := d.InsertStepRound(step.ID, i+1, []string{"initial", "auto_fix"}[i], &raw, nil, 10); err != nil {
			t.Fatal(err)
		}
	}
	got, err := d.StepFindingStats(step)
	if err != nil || got.ReportedFindings != 2 || got.FixedFindings != 1 {
		t.Fatalf("support claims merged or repeated by pointer identity: stats=%+v err=%v", got, err)
	}
}
