package daemon

import (
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"testing"
)

func gateSnapshot(e interface{ CurrentGate() types.GateIdentity }) *types.GateIdentity {
	g := e.CurrentGate()
	return &g
}

func databaseGateSnapshot(t *testing.T, d *db.DB, stepID string) types.GateIdentity {
	t.Helper()
	row, err := d.GetStepResult(stepID)
	if err != nil || row == nil {
		t.Fatalf("gate snapshot: %v, %v", row, err)
	}
	return row.GateIdentity()
}

func managerGateSnapshot(t *testing.T, m *RunManager, runID string, step types.StepName) *types.GateIdentity {
	t.Helper()
	rows, err := m.db.GetStepsByRun(runID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.StepName == step {
			g := row.GateIdentity()
			return &g
		}
	}
	return nil
}

func ipcGateSnapshot(t *testing.T, client *ipc.Client, runID string, step types.StepName) *types.GateIdentity {
	t.Helper()
	var result ipc.GetRunResult
	if err := client.Call(ipc.MethodGetRun, &ipc.GetRunParams{RunID: runID}, &result); err != nil {
		t.Fatal(err)
	}
	if result.Run != nil {
		for _, row := range result.Run.Steps {
			if row.StepName == step {
				return row.Gate
			}
		}
	}
	return nil
}
