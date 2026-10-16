package pipeline

import (
	"encoding/json"
	"fmt"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

// parkGateFixture uses the production park transaction so recovery fixtures
// carry exactly the identity a real executor publishes.
func parkGateFixture(d *db.DB, stepID string, status types.StepStatus, duration int64) error {
	row, err := d.GetStepResult(stepID)
	if err != nil {
		return err
	}
	if row == nil {
		return fmt.Errorf("missing fixture step %s", stepID)
	}
	exit := 0
	if row.ExitCode != nil {
		exit = *row.ExitCode
	}
	return d.ParkStepForApproval(row.RunID, row.ID, status, exit, duration, row.FindingsJSON)
}

func boundPendingFixture(t *testing.T, d *db.DB, stepID, raw string) string {
	t.Helper()
	row, err := d.GetStepResult(stepID)
	if err != nil || row == nil {
		t.Fatalf("pending fixture step: %+v, %v", row, err)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		t.Fatal(err)
	}
	payload["gate"] = row.GateIdentity()
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}
