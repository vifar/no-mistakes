package db

import (
	"reflect"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGetRoundsByRun(t *testing.T) {
	d := openTestDB(t)
	repo, err := d.InsertRepo(t.TempDir(), "https://example.com/repo.git", "main")
	if err != nil {
		t.Fatal(err)
	}
	var runs []*Run
	var steps []*StepResult
	for _, branch := range []string{"feature", "other"} {
		run, err := d.InsertRun(repo.ID, branch, "head", "base")
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, run)
		for _, name := range []types.StepName{types.StepReview, types.StepLint} {
			step, err := d.InsertStepResult(run.ID, name)
			if err != nil {
				t.Fatal(err)
			}
			steps = append(steps, step)
			for _, n := range []int{2, 1} {
				if _, err := d.InsertStepRound(step.ID, n, "initial", nil, nil, 10); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	rounds, err := d.GetRoundsByRun(runs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rounds) != 4 {
		t.Fatalf("got %d rounds, want 4 from only the requested run", len(rounds))
	}
	byStep := make(map[string][]*StepRound)
	for i, round := range rounds {
		if i > 0 {
			previous := rounds[i-1]
			if previous.StepResultID > round.StepResultID || (previous.StepResultID == round.StepResultID && previous.Round > round.Round) {
				t.Fatalf("rounds are out of order: %+v, %+v", previous, round)
			}
		}
		byStep[round.StepResultID] = append(byStep[round.StepResultID], round)
	}
	for _, step := range steps[:2] {
		want, err := d.GetRoundsByStep(step.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(byStep[step.ID], want) {
			t.Fatalf("batched rounds differ for step %s", step.ID)
		}
	}
	if missing, err := d.GetRoundsByRun("missing"); err != nil || len(missing) != 0 {
		t.Fatalf("missing run rounds = %v, %v", missing, err)
	}
	plan := roundsQueryPlan(t, d, "EXPLAIN QUERY PLAN "+roundsByRunQuery)
	t.Logf("GetRoundsByRun query plan: %s", plan)
	if !strings.Contains(plan, "USING INDEX idx_step_rounds_step_result_round") || strings.Contains(strings.ToUpper(plan), "SCAN STEP_ROUNDS") || strings.Contains(strings.ToUpper(plan), "TEMP B-TREE") {
		t.Fatalf("batched query must use the rounds index without a full scan or sort: %s", plan)
	}
	if !strings.Contains(plan, "INDEX idx_step_results_run_order") || strings.Contains(strings.ToUpper(plan), "SCAN STEP_RESULTS") {
		t.Fatalf("batched query must find the run's steps through the step results index: %s", plan)
	}
}
