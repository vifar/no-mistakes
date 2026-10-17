package daemon

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestRunToInfoRoundStatsMatchPerStep(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"one","severity":"warning","description":"first issue"},{"id":"two","severity":"warning","description":"second issue"}]}`
	remaining := `{"findings":[{"id":"two","severity":"warning","description":"second issue"}]}`
	summary := "fix first issue"
	for _, name := range []types.StepName{types.StepReview, types.StepLint, types.StepTest, types.StepDocument} {
		step, err := d.InsertStepResult(run.ID, name)
		if err != nil {
			t.Fatal(err)
		}
		if name == types.StepTest {
			if err := d.SetStepFindings(step.ID, findings); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if name == types.StepDocument {
			continue
		}
		// Insert out of order to prove each step is aggregated in round order.
		last, err := d.InsertStepRound(step.ID, 3, "initial", &remaining, nil, 30)
		if err != nil {
			t.Fatal(err)
		}
		selected := `["two"]`
		if err := d.SetStepRoundSelection(last.ID, &selected, db.RoundSelectionSourceAutoFix); err != nil {
			t.Fatal(err)
		}
		if _, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 10); err != nil {
			t.Fatal(err)
		}
		trigger := "auto_fix"
		var fixSummary *string = &summary
		if name == types.StepLint {
			trigger, fixSummary = "user_fix", nil
		}
		if _, err := d.InsertStepRound(step.ID, 2, trigger, nil, fixSummary, 20); err != nil {
			t.Fatal(err)
		}
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := runToInfo(d, run, nil)
	for _, step := range steps {
		want.Steps = append(want.Steps, stepToInfo(d, step))
	}
	got := runToInfo(d, run, steps)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("run info differs from per-step reads:\ngot: %+v\nwant: %+v", got.Steps, want.Steps)
	}
	for _, step := range got.Steps {
		switch step.StepName {
		case types.StepReview, types.StepLint:
			if step.ReportedFindings != 2 || step.FixedFindings != 1 || step.RoundCount != 3 || step.FixRoundCount != 1 || step.PendingFixSource != db.RoundSelectionSourceAutoFix || len(step.FixSummaries) != 1 {
				t.Fatalf("unexpected round stats: %+v", step)
			}
		case types.StepTest:
			if step.ReportedFindings != 2 || step.FixedFindings != 0 || step.RoundCount != 0 {
				t.Fatalf("lost findings without rounds: %+v", step)
			}
		}
	}
}

func TestRunToInfoIncludesImmutableSubmittedHead(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "submitted-head", "base-head")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	if err := d.UpdateRunHeadSHA(run.ID, "pipeline-fix-head"); err != nil {
		t.Fatalf("advance run head: %v", err)
	}
	run, err = d.GetRun(run.ID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}

	info := runToInfo(d, run, nil)
	if info.HeadSHA != "pipeline-fix-head" {
		t.Fatalf("head = %q, want pipeline-fix-head", info.HeadSHA)
	}
	if info.SubmittedHeadSHA == nil || *info.SubmittedHeadSHA != "submitted-head" {
		t.Fatalf("submitted head = %v, want submitted-head", info.SubmittedHeadSHA)
	}
	step, err := d.InsertStepResult(run.ID, types.StepCI)
	if err != nil {
		t.Fatal(err)
	}
	const reason = "provider unavailable"
	if err := d.CompleteSkippedStep(step.ID, 0, 10, "ci.log", reason); err != nil {
		t.Fatal(err)
	}
	results, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	info = runToInfo(d, run, results)
	if len(info.Steps) != 1 || info.Steps[0].SkipReason != reason {
		t.Fatalf("IPC lost automatic skip cause: %+v", info.Steps)
	}
}

func TestRunToInfoKeepsCIOverrideReasonCISpecific(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	run := &db.Run{ID: "run-1", Status: types.RunCompleted}
	testReason := "configured test command failed"
	ciReason := "live checks still failing: required-check"
	steps := []*db.StepResult{
		{ID: "test", RunID: run.ID, StepName: types.StepTest, OverrideReason: &testReason},
		{ID: "ci", RunID: run.ID, StepName: types.StepCI, OverrideReason: &ciReason},
	}

	info := runToInfo(d, run, steps)
	if info.CIOverrideReason != ciReason {
		t.Fatalf("CIOverrideReason = %q, want %q", info.CIOverrideReason, ciReason)
	}
}

func TestStepToInfoIncludesFixSummaries(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}

	findings := `{"findings":[{"id":"review-1","severity":"warning","description":"x"}],"summary":"1"}`
	if _, err := d.InsertStepRound(step.ID, 1, "initial", &findings, nil, 100); err != nil {
		t.Fatalf("insert round 1: %v", err)
	}
	sum := "handle nil pointer in executor"
	if _, err := d.InsertStepRound(step.ID, 2, "auto_fix", nil, &sum, 100); err != nil {
		t.Fatalf("insert round 2: %v", err)
	}

	info := stepToInfo(d, step)
	if len(info.FixSummaries) != 1 || info.FixSummaries[0] != sum {
		t.Errorf("fix summaries = %v, want [%q]", info.FixSummaries, sum)
	}
}

func TestStepToInfoLabelsCombinedHousekeepingScope(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepDocument)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if _, err := d.InsertAgentInvocation(db.AgentInvocation{
		RunID: run.ID, StepName: string(types.StepDocument), Round: 1,
		Purpose: "housekeeping", Agent: "codex", SessionMode: db.InvocationModeCold,
		StartedAt: 1, CompletedAt: 2, DurationMS: 1000, ExitStatus: "ok",
	}); err != nil {
		t.Fatalf("insert invocation: %v", err)
	}

	info := stepToInfo(d, step)
	if info.WorkScope != ipc.WorkScopeDocumentLintHousekeeping {
		t.Fatalf("work scope = %q, want combined housekeeping attribution", info.WorkScope)
	}
}

func TestStepToInfoNoFixSummariesWithoutFixRounds(t *testing.T) {
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer d.Close()

	repo, err := d.InsertRepo("/home/user/project", "git@github.com:user/project.git", "main")
	if err != nil {
		t.Fatalf("insert repo: %v", err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "abc", "def")
	if err != nil {
		t.Fatalf("insert run: %v", err)
	}
	step, err := d.InsertStepResult(run.ID, types.StepLint)
	if err != nil {
		t.Fatalf("insert step: %v", err)
	}
	if _, err := d.InsertStepRound(step.ID, 1, "initial", nil, nil, 100); err != nil {
		t.Fatalf("insert round: %v", err)
	}

	info := stepToInfo(d, step)
	if len(info.FixSummaries) != 0 {
		t.Errorf("fix summaries = %v, want none", info.FixSummaries)
	}
}

// One unreadable round must only cost its own step's round statistics, as it
// did before rounds were loaded once per run.
func TestRunToInfoIsolatesUnreadableRounds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	run, err := d.InsertRun(repo.ID, "feature", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[{"id":"one","severity":"warning","description":"first issue"}]}`
	healthy, err := d.InsertStepResult(run.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.InsertStepRound(healthy.ID, 1, "initial", &findings, nil, 10); err != nil {
		t.Fatal(err)
	}
	broken, err := d.InsertStepResult(run.ID, types.StepLint)
	if err != nil {
		t.Fatal(err)
	}
	bad, err := d.InsertStepRound(broken.ID, 1, "initial", &findings, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	if _, err := raw.Exec(`UPDATE step_rounds SET duration_ms = 'unreadable' WHERE id = ?`, bad.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.GetRoundsByStep(broken.ID); err == nil {
		t.Fatal("corrupted round still scans; the fixture does not exercise the error path")
	}
	steps, err := d.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	got := runToInfo(d, run, steps)
	for _, step := range got.Steps {
		switch step.StepName {
		case types.StepReview:
			if step.RoundCount != 1 || step.ReportedFindings != 1 {
				t.Fatalf("healthy step lost round stats because another step's round is unreadable: %+v", step)
			}
		case types.StepLint:
			if step.RoundCount != 0 {
				t.Fatalf("unreadable step reported round stats: %+v", step)
			}
		}
	}
}
