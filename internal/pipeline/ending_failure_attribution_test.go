package pipeline

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"modernc.org/sqlite"
)

func TestEndingIndependentEarlyFailureRetainsCancellation(t *testing.T) {
	for _, stage := range []string{"insert step", "skip recovered step"} {
		t.Run(stage, func(t *testing.T) {
			d, p, run, repo := setupTest(t)
			dir := completionFixture(t, d, run)
			step := newPassStep(types.StepReview)
			if stage == "skip recovered step" {
				if _, err := d.InsertStepResult(run.ID, step.Name()); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(nil)
			event := &types.RunEndingEvent{Event: "run_cancel_requested", Caller: "early-failure-caller", Client: "axi", Reason: "explicit early stop", AttributionSource: "caller"}
			function := "ending_early_" + run.ID
			if err := sqlite.RegisterScalarFunction(function, 0, func(*sqlite.FunctionContext, []driver.Value) (driver.Value, error) {
				cancel(&RunCancelCause{Reason: types.RunCancelReasonAbortedByUser, Event: event})
				return nil, errors.New("independent step storage failure")
			}); err != nil {
				t.Fatal(err)
			}
			d.Close()
			var err error
			d, err = db.Open(p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			control, err := sql.Open("sqlite", p.DB())
			if err != nil {
				t.Fatal(err)
			}
			defer control.Close()
			operation := "INSERT ON step_results"
			if stage == "skip recovered step" {
				operation = "UPDATE OF status ON step_results WHEN NEW.status='skipped'"
			}
			if _, err := control.Exec(fmt.Sprintf("CREATE TRIGGER ending_early_failure BEFORE %s BEGIN SELECT %s(); END", operation, function)); err != nil {
				t.Fatal(err)
			}
			executor := NewExecutor(d, p, nil, nil, []Step{step}, nil)
			if stage == "insert step" {
				err = executor.Execute(ctx, run, repo, dir)
			} else {
				err = executor.skipRecoveredRemainder(ctx, run, repo, dir, 0)
			}
			if err == nil || !strings.Contains(err.Error(), "independent step storage failure") {
				t.Fatalf("lost independent error: %v", err)
			}
			if context.Cause(ctx) == nil || step.callCount() != 0 {
				t.Fatal("fixture did not cancel before any step executed")
			}
			stored, err := d.GetRun(run.ID)
			if err != nil || stored.Status != types.RunFailed || stored.Error == nil || !strings.Contains(*stored.Error, "independent step storage failure") {
				t.Fatalf("independent outcome changed: %+v, %v", stored, err)
			}
			if stored.EndingEvent == nil || stored.EndingEvent.Caller != event.Caller || stored.EndingEvent.Reason != event.Reason || stored.EndingEvent.Client != event.Client {
				t.Fatalf("accepted cancellation lost: %+v", stored.EndingEvent)
			}
		})
	}
}

func TestEndingFallbackSettlementRetainsCancellation(t *testing.T) {
	d, _, run, _ := setupTest(t)
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(&RunCancelCause{Reason: types.RunCancelReasonAbortedByUser, Event: &types.RunEndingEvent{Event: "run_cancel_requested", Caller: "fallback-caller", Reason: "explicit fallback stop", AttributionSource: "caller"}})
	if err := RecordRunFailure(ctx, d, run.ID, "independent panic", nil); err != nil {
		t.Fatal(err)
	}
	stored, err := d.GetRun(run.ID)
	if err != nil || stored.Status != types.RunFailed || stored.Error == nil || *stored.Error != "independent panic" {
		t.Fatalf("fallback outcome changed: %+v, %v", stored, err)
	}
	if stored.EndingEvent == nil || stored.EndingEvent.Caller != "fallback-caller" || stored.EndingEvent.Reason != "explicit fallback stop" {
		t.Fatalf("fallback lost cancellation: %+v", stored.EndingEvent)
	}
}
