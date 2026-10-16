package daemon

import (
	"context"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/pipeline"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func TestGateResponseReservesPriorityBeforeAcknowledgement(t *testing.T) {
	for _, status := range []types.StepStatus{types.StepStatusAwaitingApproval, types.StepStatusFixReview} {
		for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip, types.ActionAnswer} {
			t.Run(string(status)+"/"+string(action), func(t *testing.T) {
				p := paths.WithRoot(t.TempDir())
				if err := p.EnsureDirs(); err != nil {
					t.Fatal(err)
				}
				database, err := db.Open(p.DB())
				if err != nil {
					t.Fatal(err)
				}
				defer database.Close()
				repo, err := database.InsertRepoWithID("priority", t.TempDir(), "https://example.com/test/repo", "main")
				if err != nil {
					t.Fatal(err)
				}
				run, err := database.InsertRun(repo.ID, "feature", "head", "base")
				if err != nil {
					t.Fatal(err)
				}
				if err := database.UpdateRunStatus(run.ID, types.RunRunning); err != nil {
					t.Fatal(err)
				}
				row, err := database.InsertStepResult(run.ID, types.StepReview)
				if err != nil {
					t.Fatal(err)
				}
				if err := database.StartStep(row.ID); err != nil {
					t.Fatal(err)
				}
				findings := `{"findings":[],"summary":"parked"}`
				if _, err := database.InsertReviewStepRound(row.ID, 1, "initial", &findings, nil, run.HeadSHA, 1); err != nil {
					t.Fatal(err)
				}
				if err := database.ParkStepForApproval(run.ID, row.ID, status, 0, 1, &findings); err != nil {
					t.Fatal(err)
				}
				run, err = database.GetRun(run.ID)
				if err != nil {
					t.Fatal(err)
				}
				h := newSchedHarness(1, false)
				parked, _ := h.tryAdmit(run.ID)
				parked.Yield()
				blocker, _ := h.tryAdmit("blocker")
				h.enqueue("new")
				executor := pipeline.NewExecutor(database, p, nil, nil, []pipeline.Step{newSchedStep()}, nil)
				executor.SetRunSlot(parked)
				if err := executor.RegisterRecoveredGate(run); err != nil {
					t.Fatal(err)
				}
				acknowledged := make(chan error, 1)
				go func() { acknowledged <- executor.Respond(types.StepReview, action, nil, gateSnapshot(executor)) }()
				select {
				case err := <-acknowledged:
					if err != nil {
						t.Fatal(err)
					}
				case <-time.After(3 * time.Second):
					blocker.Release()
					t.Fatal("gate response waited for capacity")
				}
				blocker.Release()
				if got := h.startedIDs(); len(got) != 0 {
					t.Fatalf("queued work overtook an acknowledged response: %v", got)
				}
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := parked.Acquire(ctx); err != nil {
					t.Fatal(err)
				}
				parked.Release()
				if got := h.startedIDs(); !equalIDs(got, []string{"new"}) {
					t.Fatalf("queued work did not start after the resumed run: %v", got)
				}
			})
		}
	}
}
