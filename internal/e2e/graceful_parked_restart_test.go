//go:build e2e

package e2e

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

// A deliberate daemon shutdown must preserve an unanswered approval gate,
// including its worktree, so a later start resumes the same run. A shutdown
// that terminalizes the run or deletes its worktree rejects this contract.
func TestGracefulDaemonRestartPreservesParkedReviewOnRealBinary(t *testing.T) {
	const branch = "feature/graceful-parked-restart"
	h := NewHarness(t, SetupOpts{
		Agent:    "claude",
		Scenario: writeReviewAgentsRoutingScenario(t),
	})
	if out, err := h.Run("init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	h.CommitChange(branch, "feature.txt", "seed for review-agents routing\n", "add restart probe")
	h.PushToGate(branch)
	parked := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if out, err := h.Run("daemon", "stop", "--force"); err != nil {
		t.Fatalf("stop parked daemon: %v\n%s", err, out)
	} else {
		t.Logf("stop output: %s", strings.TrimSpace(out))
	}
	database, err := db.OpenReadOnly(h.NMHome + "/state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := database.GetRun(parked.ID)
	database.Close()
	if err != nil || stored.WorktreeDir == nil || *stored.WorktreeDir == "" {
		t.Fatalf("parked run has no recorded worktree: %v", err)
	}
	if stored.Status != types.RunRunning || stored.AwaitingAgentSince == nil {
		t.Fatalf("shutdown terminalized or unparked the run: %+v", stored)
	}
	if _, err := os.Stat(*stored.WorktreeDir); err != nil {
		t.Fatalf("shutdown removed parked worktree: %v", err)
	}
	if out, err := h.Run("daemon", "start"); err != nil {
		t.Fatalf("restart parked daemon: %v\n%s", err, out)
	}
	resumed := waitForStepStatus(t, h, branch, types.StepReview, types.StepStatusAwaitingApproval, 90*time.Second)
	if resumed.ID != parked.ID || resumed.Status != types.RunRunning || resumed.HeadSHA != parked.HeadSHA {
		t.Fatalf("restart replaced or terminalized the parked run: before=%+v after=%+v", parked, resumed)
	}
	step, ok := findStep(resumed.Steps, types.StepReview)
	if !ok || step.FindingsJSON == nil || !strings.Contains(*step.FindingsJSON, "routing-check") {
		t.Fatalf("restart lost unanswered review finding: %+v", step)
	}
	t.Logf("same run %s resumed at review with head %s and unanswered routing-check; worktree retained", resumed.ID, resumed.HeadSHA)
	// Registration follows daemon readiness; wait for the recovered executor
	// to accept the operator's response without dispatching a replacement run.
	deadline := time.Now().Add(15 * time.Second)
	for {
		err := h.respondError(parked.ID, types.StepReview, types.ActionFix, []string{"routing-check"})
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered gate never accepted the response: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	completed := h.WaitForRun(branch, 120*time.Second)
	if completed.ID != parked.ID || completed.Status != types.RunCompleted {
		t.Fatalf("resumed run did not complete: %+v", completed)
	}
	t.Logf("original run %s completed after the explicit fix response", completed.ID)
}
