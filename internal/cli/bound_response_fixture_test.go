package cli

import (
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func databaseGateSnapshot(t *testing.T, d *db.DB, stepID string) types.GateIdentity {
	t.Helper()
	row, err := d.GetStepResult(stepID)
	if err != nil || row == nil {
		t.Fatalf("gate snapshot: %v, %v", row, err)
	}
	return row.GateIdentity()
}
