package cli

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	toon "github.com/toon-format/toon-go"
)

func TestAxiRespond_BindsStatusSnapshotAndReportsCurrentGate(t *testing.T) {
	shown := types.GateIdentity{RunID: "run-timeout", StepResultID: "step-1", Step: types.StepReview, RoundID: "round-a", Nonce: "nonce-a", Status: types.StepStatusAwaitingApproval}
	current := shown
	current.RoundID = "round-b"
	current.Nonce = "nonce-b"
	received := make(chan ipc.RespondParams, 1)
	fx := newAxiTimeoutFixture(t, axiTimeoutOpts{respond: func(_ context.Context, raw json.RawMessage) (interface{}, error) {
		var params ipc.RespondParams
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
		received <- params
		return &ipc.RespondResult{OK: false, Code: "gate_superseded", Refusal: "gate already resolved or superseded", CurrentGate: &current}, nil
	}})
	fx.setGetRun(func(context.Context, int) (*ipc.RunInfo, error) {
		r := fx.awaiting()
		r.Steps[0].Gate = &shown
		return r, nil
	})
	out, err := executeCmd("axi", "respond", "--run", shown.RunID, "--action", "approve", "--wait", "3s")
	var ee *exitError
	if !errors.As(err, &ee) || ee.code != 2 {
		t.Fatalf("refusal = %v\n%s", err, out)
	}
	got := <-received
	if got.ExpectedGate == nil || *got.ExpectedGate != shown {
		t.Errorf("response did not bind the status snapshot: %+v", got)
	}
	for _, want := range []string{"gate_superseded", "current_gate:", "run_id: run-timeout", "step: review", "round_id: round-b", "nonce: nonce-b", "status: awaiting_approval"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal lacks %q:\n%s", want, out)
		}
	}
}

func TestAxiStatus_ExposesParkIdentity(t *testing.T) {

	repoDir, _, database, repo := setupAxiQueryRepo(t)
	chdir(t, repoDir)
	r, err := database.InsertRun(repo.ID, "main", "head", "base")
	if err != nil {
		t.Fatal(err)
	}
	if err := database.UpdateRunStatus(r.ID, types.RunRunning); err != nil {
		t.Fatal(err)
	}
	row, err := database.InsertStepResult(r.ID, types.StepReview)
	if err != nil {
		t.Fatal(err)
	}
	findings := `{"findings":[],"summary":"review"}`
	round, err := database.InsertStepRound(row.ID, 1, "initial", &findings, nil, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.ParkStepForApproval(r.ID, row.ID, types.StepStatusAwaitingApproval, 0, 1, &findings); err != nil {
		t.Fatal(err)
	}
	row, err = database.GetStepResult(row.ID)
	if err != nil || row.GateNonce == nil {
		t.Fatalf("parked row: %+v, %v", row, err)
	}
	out := axiStatusOutput(t, r.ID)
	var doc struct {
		Gate struct {
			RoundID string `toon:"round_id"`
			Nonce   string `toon:"nonce"`
		} `toon:"gate"`
	}
	if err := toon.UnmarshalString(out, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Gate.RoundID != round.ID || doc.Gate.Nonce != *row.GateNonce {
		t.Fatalf("status identity = %+v:\n%s", doc.Gate, out)
	}
}
