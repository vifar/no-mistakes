package pipeline

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

func nextIdentityPark(t *testing.T, parks <-chan ipc.Event) {
	t.Helper()
	select {
	case <-parks:
	case <-time.After(5 * time.Second):
		t.Fatal("gate did not park")
	}
}

// Both rounds use the same step result. Stale answers must not spend another
// round or release the later gate; a response naming that later gate can.
func TestExecutor_ResponseBoundToParkedRound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	parks := make(chan ipc.Event, 4)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{NeedsApproval: true, Findings: gateFindingsThree}, nil
	}}
	e := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			parks <- event
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	workDir := completionFixture(t, database, run)
	go func() { done <- e.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("executor did not stop")
		}
	})
	nextIdentityPark(t, parks)
	a := e.CurrentGate()
	if err := e.Respond(types.StepReview, types.ActionAnswer, nil, &a); err != nil {
		t.Fatal(err)
	}
	nextIdentityPark(t, parks)
	b := e.CurrentGate()
	for _, action := range []types.ApprovalAction{types.ActionApprove, types.ActionFix, types.ActionSkip, types.ActionAnswer} {
		err := e.Respond(types.StepReview, action, nil, nil)
		var refusal *GateSupersededError
		if !errors.As(err, &refusal) || refusal.Current != b {
			t.Fatalf("unbound %s response: %v", action, err)
		}
	}
	err := e.Respond(types.StepReview, types.ActionAnswer, nil, &a)
	var stale *GateSupersededError
	if !errors.As(err, &stale) {
		t.Fatalf("stale round accepted or not typed: %v", err)
	}
	if stale.Current != b {
		t.Fatalf("current identity = %+v, want %+v", stale.Current, b)
	}
	rows, _ := database.GetStepsByRun(run.ID)
	rounds, _ := database.GetRoundsByStep(rows[0].ID)
	if len(rounds) != 2 {
		t.Fatalf("round count = %d, want 2", len(rounds))
	}
	for _, field := range []string{"run", "step-result", "step", "round", "nonce"} {
		t.Run(field, func(t *testing.T) {
			bad := b
			switch field {
			case "run":
				bad.RunID += "-stale"
			case "step-result":
				bad.StepResultID += "-stale"
			case "step":
				bad.Step = types.StepTest
			case "round":
				bad.RoundID += "-stale"
			case "nonce":
				bad.Nonce += "-stale"
			}
			err := e.Respond(types.StepReview, types.ActionApprove, nil, &bad)
			var refusal *GateSupersededError
			if !errors.As(err, &refusal) || refusal.Current != b {
				t.Fatalf("identity mismatch not refused with current gate: %v", err)
			}
		})
	}
	if err := e.Respond(types.StepReview, types.ActionApprove, nil, &b); err != nil {
		t.Fatalf("current gate refused: %v", err)
	}
}

func TestExecutor_ReparkSameRoundChangesNonce(t *testing.T) {
	database, p, run, _ := setupTest(t)
	row, recovered := seedRecoveredReviewGate(t, database, run, gateFindingsThree, types.StepStatusAwaitingApproval, "")
	step := &adaptiveCallStep{name: types.StepReview}
	first := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	if err := first.RegisterRecoveredGate(recovered); err != nil {
		t.Fatal(err)
	}
	a := first.CurrentGate()
	if err := database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, gateIdentityStringPtr(gateFindingsThree)); err != nil {
		t.Fatal(err)
	}
	second := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	if err := second.RegisterRecoveredGate(recovered); err != nil {
		t.Fatal(err)
	}
	b := second.CurrentGate()
	if a.Nonce == "" || b.Nonce == "" || a.Nonce == b.Nonce || a.RoundID != b.RoundID {
		t.Fatalf("repark identity did not renew nonce: before=%+v after=%+v", a, b)
	}
	var stale *GateSupersededError
	if err := second.Respond(types.StepReview, types.ActionAnswer, nil, &a); !errors.As(err, &stale) || stale.Current != b {
		t.Fatalf("old nonce refusal = %v, current = %+v", err, stale)
	}
	if err := second.Respond(types.StepReview, types.ActionAbort, nil, &a); err != nil {
		t.Fatalf("abort must ignore stale identity: %v", err)
	}
}

func TestExecutor_RecoveredPendingResponseRejectsSupersededGate(t *testing.T) {
	database, p, run, _ := setupTest(t)
	row, recovered := seedRecoveredReviewGate(t, database, run, gateFindingsThree, types.StepStatusAwaitingApproval, "")
	step := &adaptiveCallStep{name: types.StepReview}
	old := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	if err := old.RegisterRecoveredGate(recovered); err != nil {
		t.Fatal(err)
	}
	old.SetRunSlot(&recordingSlot{})
	if err := old.Respond(types.StepReview, types.ActionAnswer, nil, gateSnapshot(old)); err != nil {
		t.Fatal(err)
	}
	accepted, _ := database.GetStepResult(row.ID)
	if accepted.PendingResponse == nil {
		t.Fatal("response was not persisted")
	}
	raw := *accepted.PendingResponse
	if err := database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, gateIdentityStringPtr(gateFindingsThree)); err != nil {
		t.Fatal(err)
	}
	// Model a stale saved response restored beside a later park (same round).
	if err := database.SetStepPendingResponse(row.ID, raw, answeredActivity, databaseGateSnapshot(t, database, row.ID)); err != nil {
		t.Fatal(err)
	}
	fresh := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	err := fresh.RegisterRecoveredGate(recovered)
	var stale *GateSupersededError
	if !errors.As(err, &stale) {
		t.Fatalf("stale recovered response admitted: %v", err)
	}
	if stale.Current.RunID != run.ID || stale.Current.RoundID == "" || stale.Current.Nonce == "" {
		t.Fatalf("refusal lacks current identity: %+v", stale.Current)
	}
}

func TestExecutor_PendingResponseStoresFullIdentity(t *testing.T) {
	database, p, run, _ := setupTest(t)
	row, recovered := seedRecoveredReviewGate(t, database, run, gateFindingsThree, types.StepStatusAwaitingApproval, "")
	e := NewExecutor(database, p, nil, nil, []Step{&adaptiveCallStep{name: types.StepReview}}, nil)
	if err := e.RegisterRecoveredGate(recovered); err != nil {
		t.Fatal(err)
	}
	want := e.CurrentGate()
	e.SetRunSlot(&recordingSlot{})
	if err := e.Respond(types.StepReview, types.ActionAnswer, nil, &want); err != nil {
		t.Fatal(err)
	}
	saved, _ := database.GetStepResult(row.ID)
	var payload struct {
		Gate types.GateIdentity `json:"gate"`
	}
	if err := json.Unmarshal([]byte(*saved.PendingResponse), &payload); err != nil {
		t.Fatal(err)
	}
	if !want.SameGate(payload.Gate) {
		t.Fatalf("pending response identity = %+v, want %+v", payload.Gate, want)
	}
}

func gateIdentityStringPtr(s string) *string { return &s }

// The final acceptance boundary checks durable identity as well as the live
// snapshot. This covers both immediate and slot-backed admission.
func TestExecutor_SlotAcceptanceRejectsReparkedGate(t *testing.T) {
	for _, slotted := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "slot"}[slotted], func(t *testing.T) {
			database, p, run, _ := setupTest(t)
			row, recovered := seedRecoveredReviewGate(t, database, run, gateFindingsThree, types.StepStatusAwaitingApproval, "")
			e := NewExecutor(database, p, nil, nil, []Step{&adaptiveCallStep{name: types.StepReview}}, nil)
			if err := e.RegisterRecoveredGate(recovered); err != nil {
				t.Fatal(err)
			}
			if slotted {
				e.SetRunSlot(&recordingSlot{})
			}
			old := e.CurrentGate()
			if err := database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, gateIdentityStringPtr(gateFindingsThree)); err != nil {
				t.Fatal(err)
			}
			current, _ := database.GetStepResult(row.ID)
			err := e.Respond(types.StepReview, types.ActionAnswer, nil, &old)
			var stale *GateSupersededError
			if !errors.As(err, &stale) || stale.Current != current.GateIdentity() {
				t.Fatalf("durable repark was accepted: %v", err)
			}
			saved, _ := database.GetStepResult(row.ID)
			if saved.PendingResponse != nil || saved.Status != types.StepStatusAwaitingApproval {
				t.Fatalf("stale acceptance changed the new gate: %+v", saved)
			}
		})
	}
}

func TestExecutor_ResumeRechecksPreviouslyRegisteredPendingIdentity(t *testing.T) {
	for _, slotted := range []bool{false, true} {
		t.Run(map[bool]string{false: "immediate", true: "slot"}[slotted], func(t *testing.T) {
			database, p, run, repo := setupTest(t)
			row, recovered := seedRecoveredReviewGate(t, database, run, gateFindingsThree, types.StepStatusAwaitingApproval, "")
			calls := 0
			step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) { calls++; return nil, errors.New("unexpected stale dispatch") }}
			old := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			old.SetRunSlot(&recordingSlot{})
			if err := old.RegisterRecoveredGate(recovered); err != nil {
				t.Fatal(err)
			}
			if err := old.Respond(types.StepReview, types.ActionAnswer, nil, gateSnapshot(old)); err != nil {
				t.Fatal(err)
			}
			e := NewExecutor(database, p, nil, nil, []Step{step}, nil)
			if slotted {
				e.SetRunSlot(&recordingSlot{})
			}
			if err := e.RegisterRecoveredGate(recovered); err != nil {
				t.Fatal(err)
			}
			if err := database.ParkStepForApproval(run.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, gateIdentityStringPtr(gateFindingsThree)); err != nil {
				t.Fatal(err)
			}
			err := e.Resume(context.Background(), recovered, repo, completionFixture(t, database, run))
			var stale *GateSupersededError
			if !errors.As(err, &stale) || calls != 0 {
				t.Fatalf("registered response reapplied to another park: calls=%d err=%v", calls, err)
			}
		})
	}
}

// A failed round insert must not turn the previous durable round into this
// park's identity. Abort remains available, but the incomplete gate cannot
// authorize another dispatch.
func TestExecutor_FailedRoundWriteCannotBorrowPreviousGateRound(t *testing.T) {
	database, p, run, repo := setupTest(t)
	parks := make(chan ipc.Event, 4)
	step := &adaptiveCallStep{name: types.StepReview, fn: func(*StepContext) (*StepOutcome, error) {
		return &StepOutcome{NeedsApproval: true, Findings: gateFindingsThree}, nil
	}}
	e := NewExecutor(database, p, nil, nil, []Step{step}, func(event ipc.Event) {
		if event.Status != nil && *event.Status == string(types.StepStatusAwaitingApproval) {
			parks <- event
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	workDir := completionFixture(t, database, run)
	go func() { done <- e.Execute(ctx, run, repo, workDir) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("executor did not stop")
		}
	})
	nextIdentityPark(t, parks)
	a := e.CurrentGate()
	control, err := sql.Open("sqlite", p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer control.Close()
	if _, err := control.Exec(`CREATE TRIGGER fail_later_round BEFORE INSERT ON step_rounds WHEN NEW.round > 1 BEGIN SELECT RAISE(FAIL, 'round write failed'); END`); err != nil {
		t.Fatal(err)
	}
	if err := e.Respond(types.StepReview, types.ActionAnswer, nil, &a); err != nil {
		t.Fatal(err)
	}
	nextIdentityPark(t, parks)
	b := e.CurrentGate()
	if b.RoundID != "" {
		t.Fatalf("failed round borrowed an older round identity: before=%+v after=%+v", a, b)
	}
	var stale *GateSupersededError
	if err := e.Respond(types.StepReview, types.ActionApprove, nil, &b); !errors.As(err, &stale) {
		t.Fatalf("incomplete gate authorized a dispatch: %v", err)
	}
	if err := e.Respond(types.StepReview, types.ActionAbort, nil, &a); err != nil {
		t.Fatalf("abort was bound to the incomplete gate: %v", err)
	}
}

func TestExecutor_RecoveredPendingResponseRequiresIdentity(t *testing.T) {
	database, p, run, _ := setupTest(t)
	row, recovered := seedRecoveredReviewGate(t, database, run, gateFindingsThree, types.StepStatusAwaitingApproval, "")
	current := databaseGateSnapshot(t, database, row.ID)
	if err := database.SetStepPendingResponse(row.ID, `{"action":"approve"}`, answeredActivity, current); err != nil {
		t.Fatal(err)
	}
	current.Status = types.StepStatusRunning
	step := &adaptiveCallStep{name: types.StepReview}
	e := NewExecutor(database, p, nil, nil, []Step{step}, nil)
	err := e.RegisterRecoveredGate(recovered)
	var refusal *GateSupersededError
	if !errors.As(err, &refusal) || refusal.Current != current {
		t.Fatalf("unbound saved response admitted: %v", err)
	}
	saved, err := database.GetStepResult(row.ID)
	if err != nil || saved.Status != types.StepStatusAwaitingApproval || saved.PendingResponse == nil {
		t.Fatalf("recovery changed the gate: %+v, %v", saved, err)
	}
}
