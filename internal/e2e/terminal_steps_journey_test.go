//go:build e2e

package e2e

import (
	"context"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
	"path/filepath"
	"strings"
	"testing"
)

// Drive the actual CLI/daemon through the issue #102 legacy database shape.
// The fake agent and forge are external boundaries; Git, DB and recovery are real.
func TestAxiTerminalStalePushRecoveryJourney(t *testing.T) {
	h := NewHarness(t, SetupOpts{Agent: "claude", Scenario: branchSyncScenario(t)})
	h.CommitChange("init-stale", "seed.txt", "seed\n", "seed stale init")
	init := h.AddWorktree("init-stale")
	if out, err := h.RunInDir(init, "init"); err != nil {
		t.Fatalf("init: %v\n%s", err, out)
	}
	submitted := h.CommitChange("feature/stale-push", "feature.txt", "unsafe\n", "feature")
	operator := h.AddWorktree("feature/stale-push")
	gateOut, err := h.RunInDir(operator, "axi", "run", "--intent", "preserve unpublished fixes when cancelling")
	if err != nil || !strings.Contains(gateOut, "sync-1") {
		t.Fatalf("review: %v\n%s", err, gateOut)
	}
	if out, err := h.RunInDir(operator, "axi", "respond", "--action", "fix", "--findings", "sync-1"); err != nil {
		t.Fatalf("fix: %v\n%s", err, out)
	}
	if out, err := h.RunInDir(operator, "axi", "abort"); err != nil {
		t.Fatalf("abort: %v\n%s", err, out)
	}
	database, err := db.Open(paths.WithRoot(h.NMHome).DB())
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	run, err := database.GetActiveRun(h.repoID(), "feature/stale-push")
	if err != nil || run != nil {
		t.Fatalf("active run after abort: %+v %v", run, err)
	}
	runs, err := database.GetRunsByRepo(h.repoID())
	if err != nil {
		t.Fatal(err)
	}
	for _, candidate := range runs {
		if candidate.Branch == "feature/stale-push" {
			run = candidate
			break
		}
	}
	if run == nil || run.Status != types.RunCancelled || run.HeadSHA == submitted {
		t.Fatalf("wrong legacy fixture: %+v", run)
	}
	gateDir := filepath.Join(h.NMHome, "repos", h.repoID()+".git")
	anchor, err := h.runGit(context.Background(), gateDir, "rev-parse", custody.RecoveryRef(run.ID))
	if err != nil || strings.TrimSpace(string(anchor)) != run.HeadSHA {
		t.Fatalf("recovery ref: %s %v", anchor, err)
	}
	// Re-introduce the old persisted defect after quiescence, with no executor.
	step, err := database.InsertStepResult(run.ID, types.StepPush)
	if err != nil {
		t.Fatal(err)
	}
	if err := database.StartStep(step.ID); err != nil {
		t.Fatal(err)
	}
	status, err := h.RunInDir(operator, "axi", "status")
	if err != nil || !strings.Contains(status, "recover_custody") || strings.Contains(status, "continue_active_run") {
		t.Fatalf("status: %v\n%s", err, status)
	}
	out, err := h.RunInDir(h.HomeDir, "axi", "abort", "--run", run.ID)
	if err != nil {
		t.Fatalf("legacy abort: %v\n%s", err, out)
	}
	if !strings.Contains(out, "aborted: false") || !strings.Contains(out, "steps_closed: 1") {
		t.Fatalf("cleanup fabricated a new cancellation: %s", out)
	}
	row, err := database.GetStepResult(step.ID)
	if err != nil || row.CompletedAt == nil || row.Status != types.StepStatusFailed {
		t.Fatalf("push row not closed: %+v %v", row, err)
	}
	again, err := h.RunInDir(h.HomeDir, "axi", "abort", "--run", run.ID)
	if err != nil || !strings.Contains(again, "aborted: false") {
		t.Fatalf("repeat abort: %v\n%s", err, again)
	}
	recovered, err := h.RunInDir(operator, "axi", "sync", "--recover")
	if err != nil {
		t.Fatalf("recover: %v\n%s", err, recovered)
	}
	final, err := database.GetRun(run.ID)
	if err != nil || final.CustodyReturnedAt == nil {
		t.Fatalf("custody: %+v %v", final, err)
	}
	if got := h.WorktreeRefSHA("feature/stale-push"); got != run.HeadSHA {
		t.Fatalf("local head=%s want %s", got, run.HeadSHA)
	}
	t.Logf("status offers recovery; abort closes step %s; repeat abort is a no-op; sync --recover returns custody at %s", step.ID, run.HeadSHA)
}
