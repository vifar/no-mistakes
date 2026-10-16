package pipeline

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/types"
)

// The shipping intent requires a CI repair's own findings to park before its
// requested restart. A restart is permission to defer carried findings only;
// treating its newly reported findings the same way would skip this gate.
func TestExecutor_RestartWithOwnFindingsParksBeforeRestarting(t *testing.T) {
	database, p, run, repo := setupTest(t)
	var reviews atomic.Int32
	review := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		reviews.Add(1)
		return &StepOutcome{}, nil
	}}
	ci := &adaptiveCallStep{name: types.StepCI, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{
			RestartFrom: types.StepReview,
			Findings:    `{"findings":[{"id":"repair-defect","severity":"error","description":"repair needs a human decision","action":"ask-user"}],"summary":"unresolved repair"}`,
		}, nil
	}}
	exec := NewExecutor(database, p, nil, nil, []Step{review, ci}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	workDir := t.TempDir()
	go func() { done <- exec.Execute(ctx, run, repo, workDir) }()
	waitForStepStatus(t, database, run.ID, types.StepCI, types.StepStatusAwaitingApproval)
	if got := reviews.Load(); got != 1 {
		t.Fatalf("review executions before resolving CI's own finding = %d, want 1", got)
	}
	results, err := database.GetStepsByRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 || results[1].FindingsJSON == nil {
		t.Fatal("CI gate did not persist its findings")
	}
	parsed, err := types.ParseFindingsJSON(*results[1].FindingsJSON)
	if err != nil || len(parsed.Items) != 1 || parsed.Items[0].ID != "repair-defect" {
		t.Fatalf("CI gate findings = %#v, %v; want its own repair-defect", parsed, err)
	}
	if err := exec.Respond(types.StepCI, types.ActionAbort, nil, nil); err != nil {
		t.Fatal(err)
	}
	<-done
}
