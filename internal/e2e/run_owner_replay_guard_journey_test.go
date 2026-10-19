//go:build e2e

package e2e

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// Retained notifications are evidence, not authority to replace newer work.
// Drive the public replay CLI against a real daemon and assert that stale or
// corrupt records cannot admit a run, cancel current work, or erase the record.
func TestRunOwnerReplayRefusalJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: axiScenario(t), GlobalConfigExtra: "daemon:\n  max_concurrent_runs: 1\n  hold_slot_while_parked: true\n"})
	statusCommand(t, h, h.WorkDir, "init")
	h.CommitChange("feature/holder", "holder.txt", "hold slot\n", "holder")
	h.PushToGate("feature/holder")
	holder := waitForStepStatus(t, h, "feature/holder", types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	writer, err := sql.Open("sqlite", paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	for _, fault := range []string{"stale", "corrupt"} {
		t.Run(fault, func(t *testing.T) {
			branch := "feature/replay-" + fault
			h.CommitChange(branch, "replay.txt", "B\n", "B")
			h.PushToGate(branch)
			prior := h.ActiveRun(branch)
			if prior == nil || prior.Status != types.RunQueued {
				t.Fatalf("missing queued predecessor: %+v", prior)
			}
			if _, err := writer.Exec("CREATE TRIGGER replay_insert_fault BEFORE INSERT ON runs BEGIN SELECT RAISE(ABORT,'replay insert fault'); END"); err != nil {
				t.Fatal(err)
			}
			head := h.CommitChange(branch, "replay.txt", "C\n", "C")
			h.PushToGate(branch)
			if _, err := writer.Exec("DROP TRIGGER replay_insert_fault"); err != nil {
				t.Fatal(err)
			}
			var records []ipc.PushNotificationInfo
			if err := json.Unmarshal([]byte(statusCommand(t, h, h.WorkDir, "daemon", "notifications", "list")), &records); err != nil {
				t.Fatal(err)
			}
			var pending ipc.PushNotificationInfo
			for _, record := range records {
				if !record.Pending {
					continue
				}
				var detail ipc.PushNotificationInfo
				if err := json.Unmarshal([]byte(statusCommand(t, h, h.WorkDir, "daemon", "notifications", "inspect", record.ID)), &detail); err != nil {
					t.Fatal(err)
				}
				if detail.Params != nil && detail.Params.New == head && detail.Params.Ref == "refs/heads/"+branch {
					pending = detail
				}
			}
			if pending.ID == "" || h.ActiveRun(branch).ID != prior.ID {
				t.Fatal("failed receive did not retain evidence and predecessor")
			}
			if fault == "stale" {
				h.CommitChange(branch, "replay.txt", "D\n", "D")
				h.PushToGate(branch)
			} else if err := os.WriteFile(pending.Path, []byte("{broken"), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(pending.Path)
			if err != nil {
				t.Fatal(err)
			}
			current, count := h.ActiveRun(branch), len(h.Runs())
			out, err := h.Run("daemon", "notifications", "replay", pending.ID)
			t.Logf("replay %s id=%s: %v\n%s", fault, pending.ID, err, out)
			if err == nil || !strings.Contains(out, fault+" notification") {
				t.Fatal("unsafe replay was not refused with its reason")
			}
			after, err := os.ReadFile(pending.Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("refusal changed retained evidence: %v", err)
			}
			got := h.ActiveRun(branch)
			if len(h.Runs()) != count || got == nil || current == nil || got.ID != current.ID || got.Status != current.Status {
				t.Fatal("refused replay replaced or cancelled current work")
			}
			t.Logf("refusal preserved current run=%s status=%s and original record bytes", got.ID, got.Status)
			h.CancelRun(got.ID)
		})
	}
	h.CancelRun(holder.ID)
}
