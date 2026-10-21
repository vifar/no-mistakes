//go:build e2e

package e2e

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Drive the real CLI and daemon; the agent boundary is the harness's existing
// silent provider. The independent oracle is the exact operator metadata, read
// from SQLite and from public status after cancellation has stopped the work.
func TestEndingCLIAbortAttributionJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: silentAgentScenario(t)})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	const branch = "feature/ending-attribution"
	h.CommitChange(branch, "caller.txt", "cancel attribution\n", "add abort probe")
	h.PushToGate(branch)
	run := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusRunning, 90*time.Second)
	if out, err := h.Run("axi", "abort", "--run", run.ID, "--owner", "journey-thread-60", "--reason", "owner priority requires slot"); err != nil {
		t.Fatalf("abort: %v\n%s", err, out)
	}
	d, err := db.OpenReadOnly(h.NMHome + "/state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	stored, err := d.GetRun(run.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(stored)
	var record map[string]any
	_ = json.Unmarshal(raw, &record)
	e, _ := record["EndingEvent"].(map[string]any)
	if stored.Status != types.RunCancelled || e["caller"] != "journey-thread-60" || e["client"] != "axi" || e["reason"] != "owner priority requires slot" {
		t.Fatalf("persisted abort = %s", raw)
	}
	out, err := h.Run("axi", "status", "--run", run.ID, "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, out)
	}
	var doc struct {
		Run struct {
			Status string         `json:"status"`
			Event  map[string]any `json:"ending_event"`
		} `json:"run"`
	}
	if err = json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("status JSON: %v\n%s", err, out)
	}
	if doc.Run.Status != "cancelled" || doc.Run.Event["caller"] != "journey-thread-60" {
		t.Fatalf("status loses attribution or reports stale running: %s", out)
	}
	t.Logf("run %s stopped; SQLite and public JSON identify journey-thread-60, axi, and explicit priority reason", run.ID)
}
