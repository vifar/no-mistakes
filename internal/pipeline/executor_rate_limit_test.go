package pipeline

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/netretry"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A GitHub rate limit parks the step inside the forge call. The wait must be
// the step's visible last activity, so `axi status` says what it waits for,
// and the step must then continue rather than fail.
func TestExecutor_RateLimitWaitIsTheStepsLastActivity(t *testing.T) {
	database, p, run, repo := setupTest(t)
	workDir := completionFixture(t, database, run)

	step := &adaptiveCallStep{
		name: types.StepReview,
		fn: func(sctx *StepContext) (*StepOutcome, error) {
			calls := 0
			_, err := netretry.Do(sctx.Ctx, netretry.Options{
				Operation: "gh pr view base branch",
				Wait: func(context.Context, time.Duration) error {
					got, err := database.GetStepResult(sctx.StepResultID)
					if err != nil {
						t.Fatalf("get step result: %v", err)
					}
					if got.Status != types.StepStatusRunning || got.LastActivity == nil ||
						!strings.Contains(*got.LastActivity, "waiting for GitHub rate limit until ") {
						t.Fatalf("status=%s last_activity=%v, want the running step to report the rate-limit wait", got.Status, got.LastActivity)
					}
					return nil
				},
			}, func() (string, error) {
				calls++
				if calls == 1 {
					return "", errors.New("gh pr view base branch: GraphQL: API rate limit already exceeded for user ID 12345.")
				}
				return "main", nil
			})
			if err != nil {
				return nil, err
			}
			return &StepOutcome{ExitCode: 0}, nil
		},
	}

	exec := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	if err := exec.Execute(context.Background(), run, repo, workDir); err != nil {
		t.Fatalf("execute: %v", err)
	}
}
